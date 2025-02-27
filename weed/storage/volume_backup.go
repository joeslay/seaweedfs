package storage

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/chrislusf/seaweedfs/weed/operation"
	"github.com/chrislusf/seaweedfs/weed/pb/volume_server_pb"
	"github.com/chrislusf/seaweedfs/weed/storage/idx"
	"github.com/chrislusf/seaweedfs/weed/storage/needle"
	. "github.com/chrislusf/seaweedfs/weed/storage/types"
	"google.golang.org/grpc"
)

func (v *Volume) GetVolumeSyncStatus() *volume_server_pb.VolumeSyncStatusResponse {
	v.dataFileAccessLock.Lock()
	defer v.dataFileAccessLock.Unlock()

	var syncStatus = &volume_server_pb.VolumeSyncStatusResponse{}
	if stat, err := v.dataFile.Stat(); err == nil {
		syncStatus.TailOffset = uint64(stat.Size())
	}
	syncStatus.Collection = v.Collection
	syncStatus.IdxFileSize = v.nm.IndexFileSize()
	syncStatus.CompactRevision = uint32(v.SuperBlock.CompactionRevision)
	syncStatus.Ttl = v.SuperBlock.Ttl.String()
	syncStatus.Replication = v.SuperBlock.ReplicaPlacement.String()
	return syncStatus
}

// The volume sync with a master volume via 2 steps:
// 1. The slave checks master side to find subscription checkpoint
//	  to setup the replication.
// 2. The slave receives the updates from master

/*
Assume the slave volume needs to follow the master volume.

The master volume could be compacted, and could be many files ahead of
slave volume.

Step 0: // implemented in command/backup.go, to avoid dat file size overflow.
0.1 If slave compact version is less than the master, do a local compaction, and set
local compact version the same as the master.
0.2 If the slave size is still bigger than the master, discard local copy and do a full copy.

Step 1:
The slave volume ask the master by the last modification time t.
The master do a binary search in volume (use .idx as an array, and check the appendAtNs in .dat file),
to find the first entry with appendAtNs > t.

Step 2:
The master send content bytes to the slave. The bytes are not chunked by needle.

Step 3:
The slave generate the needle map for the new bytes. (This may be optimized to incrementally
update needle map when receiving new .dat bytes. But seems not necessary now.)

*/

func (v *Volume) IncrementalBackup(volumeServer string, grpcDialOption grpc.DialOption) error {

	ctx := context.Background()

	startFromOffset, _, _ := v.FileStat()
	appendAtNs, err := v.findLastAppendAtNs()
	if err != nil {
		return err
	}

	err = operation.WithVolumeServerClient(volumeServer, grpcDialOption, func(client volume_server_pb.VolumeServerClient) error {

		stream, err := client.VolumeIncrementalCopy(ctx, &volume_server_pb.VolumeIncrementalCopyRequest{
			VolumeId: uint32(v.Id),
			SinceNs:  appendAtNs,
		})
		if err != nil {
			return err
		}

		v.dataFile.Seek(int64(startFromOffset), io.SeekStart)

		for {
			resp, recvErr := stream.Recv()
			if recvErr != nil {
				if recvErr == io.EOF {
					break
				} else {
					return recvErr
				}
			}

			_, writeErr := v.dataFile.Write(resp.FileContent)
			if writeErr != nil {
				return writeErr
			}
		}

		return nil

	})

	if err != nil {
		return err
	}

	// add to needle map
	return ScanVolumeFileFrom(v.version, v.dataFile, int64(startFromOffset), &VolumeFileScanner4GenIdx{v: v})

}

func (v *Volume) findLastAppendAtNs() (uint64, error) {
	offset, err := v.locateLastAppendEntry()
	if err != nil {
		return 0, err
	}
	if offset.IsZero() {
		return 0, nil
	}
	return v.readAppendAtNs(offset)
}

func (v *Volume) locateLastAppendEntry() (Offset, error) {
	indexFile, e := os.OpenFile(v.FileName()+".idx", os.O_RDONLY, 0644)
	if e != nil {
		return Offset{}, fmt.Errorf("cannot read %s.idx: %v", v.FileName(), e)
	}
	defer indexFile.Close()

	fi, err := indexFile.Stat()
	if err != nil {
		return Offset{}, fmt.Errorf("file %s stat error: %v", indexFile.Name(), err)
	}
	fileSize := fi.Size()
	if fileSize%NeedleMapEntrySize != 0 {
		return Offset{}, fmt.Errorf("unexpected file %s size: %d", indexFile.Name(), fileSize)
	}
	if fileSize == 0 {
		return Offset{}, nil
	}

	bytes := make([]byte, NeedleMapEntrySize)
	n, e := indexFile.ReadAt(bytes, fileSize-NeedleMapEntrySize)
	if n != NeedleMapEntrySize {
		return Offset{}, fmt.Errorf("file %s read error: %v", indexFile.Name(), e)
	}
	_, offset, _ := idx.IdxFileEntry(bytes)

	return offset, nil
}

func (v *Volume) readAppendAtNs(offset Offset) (uint64, error) {

	n, _, bodyLength, err := needle.ReadNeedleHeader(v.dataFile, v.SuperBlock.version, offset.ToAcutalOffset())
	if err != nil {
		return 0, fmt.Errorf("ReadNeedleHeader: %v", err)
	}
	_, err = n.ReadNeedleBody(v.dataFile, v.SuperBlock.version, offset.ToAcutalOffset()+int64(NeedleHeaderSize), bodyLength)
	if err != nil {
		return 0, fmt.Errorf("ReadNeedleBody offset %d, bodyLength %d: %v", offset.ToAcutalOffset(), bodyLength, err)
	}
	return n.AppendAtNs, nil

}

// on server side
func (v *Volume) BinarySearchByAppendAtNs(sinceNs uint64) (offset Offset, isLast bool, err error) {
	indexFile, openErr := os.OpenFile(v.FileName()+".idx", os.O_RDONLY, 0644)
	if openErr != nil {
		err = fmt.Errorf("cannot read %s.idx: %v", v.FileName(), openErr)
		return
	}
	defer indexFile.Close()

	fi, statErr := indexFile.Stat()
	if statErr != nil {
		err = fmt.Errorf("file %s stat error: %v", indexFile.Name(), statErr)
		return
	}
	fileSize := fi.Size()
	if fileSize%NeedleMapEntrySize != 0 {
		err = fmt.Errorf("unexpected file %s size: %d", indexFile.Name(), fileSize)
		return
	}

	bytes := make([]byte, NeedleMapEntrySize)
	entryCount := fileSize / NeedleMapEntrySize
	l := int64(0)
	h := entryCount

	for l < h {

		m := (l + h) / 2

		if m == entryCount {
			return Offset{}, true, nil
		}

		// read the appendAtNs for entry m
		offset, err = v.readAppendAtNsForIndexEntry(indexFile, bytes, m)
		if err != nil {
			return
		}

		mNs, nsReadErr := v.readAppendAtNs(offset)
		if nsReadErr != nil {
			err = nsReadErr
			return
		}

		// move the boundary
		if mNs <= sinceNs {
			l = m + 1
		} else {
			h = m
		}

	}

	if l == entryCount {
		return Offset{}, true, nil
	}

	offset, err = v.readAppendAtNsForIndexEntry(indexFile, bytes, l)

	return offset, false, err

}

// bytes is of size NeedleMapEntrySize
func (v *Volume) readAppendAtNsForIndexEntry(indexFile *os.File, bytes []byte, m int64) (Offset, error) {
	if _, readErr := indexFile.ReadAt(bytes, m*NeedleMapEntrySize); readErr != nil && readErr != io.EOF {
		return Offset{}, readErr
	}
	_, offset, _ := idx.IdxFileEntry(bytes)
	return offset, nil
}

// generate the volume idx
type VolumeFileScanner4GenIdx struct {
	v *Volume
}

func (scanner *VolumeFileScanner4GenIdx) VisitSuperBlock(superBlock SuperBlock) error {
	return nil

}
func (scanner *VolumeFileScanner4GenIdx) ReadNeedleBody() bool {
	return false
}

func (scanner *VolumeFileScanner4GenIdx) VisitNeedle(n *needle.Needle, offset int64) error {
	if n.Size > 0 && n.Size != TombstoneFileSize {
		return scanner.v.nm.Put(n.Id, ToOffset(offset), n.Size)
	}
	return scanner.v.nm.Delete(n.Id, ToOffset(offset))
}
