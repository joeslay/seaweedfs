package needle

import (
	"errors"
	"fmt"
	"io"
	"os"

	"math"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/storage/memory_map"
	. "github.com/chrislusf/seaweedfs/weed/storage/types"
	"github.com/chrislusf/seaweedfs/weed/util"
)

const (
	FlagGzip                = 0x01
	FlagHasName             = 0x02
	FlagHasMime             = 0x04
	FlagHasLastModifiedDate = 0x08
	FlagHasTtl              = 0x10
	FlagHasPairs            = 0x20
	FlagIsChunkManifest     = 0x80
	LastModifiedBytesLength = 5
	TtlBytesLength          = 2
)

func (n *Needle) DiskSize(version Version) int64 {
	return GetActualSize(n.Size, version)
}

func (n *Needle) prepareWriteBuffer(version Version) ([]byte, uint32, int64, error) {

	writeBytes := make([]byte, 0)

	switch version {
	case Version1:
		header := make([]byte, NeedleHeaderSize)
		CookieToBytes(header[0:CookieSize], n.Cookie)
		NeedleIdToBytes(header[CookieSize:CookieSize+NeedleIdSize], n.Id)
		n.Size = uint32(len(n.Data))
		util.Uint32toBytes(header[CookieSize+NeedleIdSize:CookieSize+NeedleIdSize+SizeSize], n.Size)
		size := n.Size
		actualSize := NeedleHeaderSize + int64(n.Size)
		writeBytes = append(writeBytes, header...)
		writeBytes = append(writeBytes, n.Data...)
		padding := PaddingLength(n.Size, version)
		util.Uint32toBytes(header[0:NeedleChecksumSize], n.Checksum.Value())
		writeBytes = append(writeBytes, header[0:NeedleChecksumSize+padding]...)
		return writeBytes, size, actualSize, nil
	case Version2, Version3:
		header := make([]byte, NeedleHeaderSize+TimestampSize) // adding timestamp to reuse it and avoid extra allocation
		CookieToBytes(header[0:CookieSize], n.Cookie)
		NeedleIdToBytes(header[CookieSize:CookieSize+NeedleIdSize], n.Id)
		if len(n.Name) >= math.MaxUint8 {
			n.NameSize = math.MaxUint8
		} else {
			n.NameSize = uint8(len(n.Name))
		}
		n.DataSize, n.MimeSize = uint32(len(n.Data)), uint8(len(n.Mime))
		if n.DataSize > 0 {
			n.Size = 4 + n.DataSize + 1
			if n.HasName() {
				n.Size = n.Size + 1 + uint32(n.NameSize)
			}
			if n.HasMime() {
				n.Size = n.Size + 1 + uint32(n.MimeSize)
			}
			if n.HasLastModifiedDate() {
				n.Size = n.Size + LastModifiedBytesLength
			}
			if n.HasTtl() {
				n.Size = n.Size + TtlBytesLength
			}
			if n.HasPairs() {
				n.Size += 2 + uint32(n.PairsSize)
			}
		} else {
			n.Size = 0
		}
		util.Uint32toBytes(header[CookieSize+NeedleIdSize:CookieSize+NeedleIdSize+SizeSize], n.Size)
		writeBytes = append(writeBytes, header[0:NeedleHeaderSize]...)
		if n.DataSize > 0 {
			util.Uint32toBytes(header[0:4], n.DataSize)
			writeBytes = append(writeBytes, header[0:4]...)
			writeBytes = append(writeBytes, n.Data...)
			util.Uint8toBytes(header[0:1], n.Flags)
			writeBytes = append(writeBytes, header[0:1]...)
			if n.HasName() {
				util.Uint8toBytes(header[0:1], n.NameSize)
				writeBytes = append(writeBytes, header[0:1]...)
				writeBytes = append(writeBytes, n.Name[:n.NameSize]...)
			}
			if n.HasMime() {
				util.Uint8toBytes(header[0:1], n.MimeSize)
				writeBytes = append(writeBytes, header[0:1]...)
				writeBytes = append(writeBytes, n.Mime...)
			}
			if n.HasLastModifiedDate() {
				util.Uint64toBytes(header[0:8], n.LastModified)
				writeBytes = append(writeBytes, header[8-LastModifiedBytesLength:8]...)
			}
			if n.HasTtl() && n.Ttl != nil {
				n.Ttl.ToBytes(header[0:TtlBytesLength])
				writeBytes = append(writeBytes, header[0:TtlBytesLength]...)
			}
			if n.HasPairs() {
				util.Uint16toBytes(header[0:2], n.PairsSize)
				writeBytes = append(writeBytes, header[0:2]...)
				writeBytes = append(writeBytes, n.Pairs...)
			}
		}
		padding := PaddingLength(n.Size, version)
		util.Uint32toBytes(header[0:NeedleChecksumSize], n.Checksum.Value())
		if version == Version2 {
			writeBytes = append(writeBytes, header[0:NeedleChecksumSize+padding]...)
		} else {
			// version3
			util.Uint64toBytes(header[NeedleChecksumSize:NeedleChecksumSize+TimestampSize], n.AppendAtNs)
			writeBytes = append(writeBytes, header[0:NeedleChecksumSize+TimestampSize+padding]...)
		}

		return writeBytes, n.DataSize, GetActualSize(n.Size, version), nil
	}

	return writeBytes, 0, 0, fmt.Errorf("Unsupported Version! (%d)", version)
}

func (n *Needle) Append(w *os.File, version Version) (offset uint64, size uint32, actualSize int64, err error) {

	mMap, exists := memory_map.FileMemoryMap[w.Name()]
	if !exists {
		if end, e := w.Seek(0, io.SeekEnd); e == nil {
			defer func(w *os.File, off int64) {
				if err != nil {
					if te := w.Truncate(end); te != nil {
						glog.V(0).Infof("Failed to truncate %s back to %d with error: %v", w.Name(), end, te)
					}
				}
			}(w, end)
			offset = uint64(end)
		} else {
			err = fmt.Errorf("Cannot Read Current Volume Position: %v", e)
			return
		}
	} else {
		offset = uint64(mMap.End_of_file + 1)
	}

	bytesToWrite, size, actualSize, err := n.prepareWriteBuffer(version)

	if err == nil {
		if exists {
			mMap.WriteMemory(offset, uint64(len(bytesToWrite)), bytesToWrite)
		} else {
			_, err = w.Write(bytesToWrite)
		}
	}

	return offset, size, actualSize, err
}

func ReadNeedleBlob(r *os.File, offset int64, size uint32, version Version) (dataSlice []byte, err error) {

	dataSize := GetActualSize(size, version)
	dataSlice = make([]byte, int(dataSize))

	mMap, exists := memory_map.FileMemoryMap[r.Name()]
	if exists {
		dataSlice, err := mMap.ReadMemory(uint64(offset), uint64(dataSize))
		return dataSlice, err
	} else {
		_, err = r.ReadAt(dataSlice, offset)
		return dataSlice, err
	}
}

// ReadBytes hydrates the needle from the bytes buffer, with only n.Id is set.
func (n *Needle) ReadBytes(bytes []byte, offset int64, size uint32, version Version) (err error) {
	n.ParseNeedleHeader(bytes)
	if n.Size != size {
		return fmt.Errorf("entry not found: offset %d found id %d size %d, expected size %d", offset, n.Id, n.Size, size)
	}
	switch version {
	case Version1:
		n.Data = bytes[NeedleHeaderSize : NeedleHeaderSize+size]
	case Version2, Version3:
		err = n.readNeedleDataVersion2(bytes[NeedleHeaderSize : NeedleHeaderSize+int(n.Size)])
	}
	if err != nil && err != io.EOF {
		return err
	}
	if size > 0 {
		checksum := util.BytesToUint32(bytes[NeedleHeaderSize+size : NeedleHeaderSize+size+NeedleChecksumSize])
		newChecksum := NewCRC(n.Data)
		if checksum != newChecksum.Value() {
			return errors.New("CRC error! Data On Disk Corrupted")
		}
		n.Checksum = newChecksum
	}
	if version == Version3 {
		tsOffset := NeedleHeaderSize + size + NeedleChecksumSize
		n.AppendAtNs = util.BytesToUint64(bytes[tsOffset : tsOffset+TimestampSize])
	}
	return nil
}

// ReadData hydrates the needle from the file, with only n.Id is set.
func (n *Needle) ReadData(r *os.File, offset int64, size uint32, version Version) (err error) {
	bytes, err := ReadNeedleBlob(r, offset, size, version)
	if err != nil {
		return err
	}
	return n.ReadBytes(bytes, offset, size, version)
}

func (n *Needle) ParseNeedleHeader(bytes []byte) {
	n.Cookie = BytesToCookie(bytes[0:CookieSize])
	n.Id = BytesToNeedleId(bytes[CookieSize : CookieSize+NeedleIdSize])
	n.Size = util.BytesToUint32(bytes[CookieSize+NeedleIdSize : NeedleHeaderSize])
}

func (n *Needle) readNeedleDataVersion2(bytes []byte) (err error) {
	index, lenBytes := 0, len(bytes)
	if index < lenBytes {
		n.DataSize = util.BytesToUint32(bytes[index : index+4])
		index = index + 4
		if int(n.DataSize)+index > lenBytes {
			return fmt.Errorf("index out of range %d", 1)
		}
		n.Data = bytes[index : index+int(n.DataSize)]
		index = index + int(n.DataSize)
		n.Flags = bytes[index]
		index = index + 1
	}
	if index < lenBytes && n.HasName() {
		n.NameSize = uint8(bytes[index])
		index = index + 1
		if int(n.NameSize)+index > lenBytes {
			return fmt.Errorf("index out of range %d", 2)
		}
		n.Name = bytes[index : index+int(n.NameSize)]
		index = index + int(n.NameSize)
	}
	if index < lenBytes && n.HasMime() {
		n.MimeSize = uint8(bytes[index])
		index = index + 1
		if int(n.MimeSize)+index > lenBytes {
			return fmt.Errorf("index out of range %d", 3)
		}
		n.Mime = bytes[index : index+int(n.MimeSize)]
		index = index + int(n.MimeSize)
	}
	if index < lenBytes && n.HasLastModifiedDate() {
		if LastModifiedBytesLength+index > lenBytes {
			return fmt.Errorf("index out of range %d", 4)
		}
		n.LastModified = util.BytesToUint64(bytes[index : index+LastModifiedBytesLength])
		index = index + LastModifiedBytesLength
	}
	if index < lenBytes && n.HasTtl() {
		if TtlBytesLength+index > lenBytes {
			return fmt.Errorf("index out of range %d", 5)
		}
		n.Ttl = LoadTTLFromBytes(bytes[index : index+TtlBytesLength])
		index = index + TtlBytesLength
	}
	if index < lenBytes && n.HasPairs() {
		if 2+index > lenBytes {
			return fmt.Errorf("index out of range %d", 6)
		}
		n.PairsSize = util.BytesToUint16(bytes[index : index+2])
		index += 2
		if int(n.PairsSize)+index > lenBytes {
			return fmt.Errorf("index out of range %d", 7)
		}
		end := index + int(n.PairsSize)
		n.Pairs = bytes[index:end]
		index = end
	}
	return nil
}

func ReadNeedleHeader(r *os.File, version Version, offset int64) (n *Needle, bytes []byte, bodyLength int64, err error) {
	n = new(Needle)
	if version == Version1 || version == Version2 || version == Version3 {
		bytes = make([]byte, NeedleHeaderSize)

		mMap, exists := memory_map.FileMemoryMap[r.Name()]
		if exists {
			bytes, err = mMap.ReadMemory(uint64(offset), NeedleHeaderSize)
			if err != nil {
				return nil, bytes, 0, err
			}
		} else {
			var count int
			count, err = r.ReadAt(bytes, offset)
			if count <= 0 || err != nil {
				return nil, bytes, 0, err
			}
		}
		n.ParseNeedleHeader(bytes)
		bodyLength = NeedleBodyLength(n.Size, version)
	}

	return
}

func PaddingLength(needleSize uint32, version Version) uint32 {
	if version == Version3 {
		// this is same value as version2, but just listed here for clarity
		return NeedlePaddingSize - ((NeedleHeaderSize + needleSize + NeedleChecksumSize + TimestampSize) % NeedlePaddingSize)
	}
	return NeedlePaddingSize - ((NeedleHeaderSize + needleSize + NeedleChecksumSize) % NeedlePaddingSize)
}

func NeedleBodyLength(needleSize uint32, version Version) int64 {
	if version == Version3 {
		return int64(needleSize) + NeedleChecksumSize + TimestampSize + int64(PaddingLength(needleSize, version))
	}
	return int64(needleSize) + NeedleChecksumSize + int64(PaddingLength(needleSize, version))
}

//n should be a needle already read the header
//the input stream will read until next file entry
func (n *Needle) ReadNeedleBody(r *os.File, version Version, offset int64, bodyLength int64) (bytes []byte, err error) {

	if bodyLength <= 0 {
		return nil, nil
	}
	bytes = make([]byte, bodyLength)
	if _, err = r.ReadAt(bytes, offset); err != nil {
		return
	}

	err = n.ReadNeedleBodyBytes(bytes, version)

	return
}

func (n *Needle) ReadNeedleBodyBytes(needleBody []byte, version Version) (err error) {

	if len(needleBody) <= 0 {
		return nil
	}
	switch version {
	case Version1:
		n.Data = needleBody[:n.Size]
		n.Checksum = NewCRC(n.Data)
	case Version2, Version3:
		err = n.readNeedleDataVersion2(needleBody[0:n.Size])
		n.Checksum = NewCRC(n.Data)

		if version == Version3 {
			tsOffset := n.Size + NeedleChecksumSize
			n.AppendAtNs = util.BytesToUint64(needleBody[tsOffset : tsOffset+TimestampSize])
		}
	default:
		err = fmt.Errorf("unsupported version %d!", version)
	}
	return
}

func (n *Needle) IsGzipped() bool {
	return n.Flags&FlagGzip > 0
}
func (n *Needle) SetGzipped() {
	n.Flags = n.Flags | FlagGzip
}
func (n *Needle) HasName() bool {
	return n.Flags&FlagHasName > 0
}
func (n *Needle) SetHasName() {
	n.Flags = n.Flags | FlagHasName
}
func (n *Needle) HasMime() bool {
	return n.Flags&FlagHasMime > 0
}
func (n *Needle) SetHasMime() {
	n.Flags = n.Flags | FlagHasMime
}
func (n *Needle) HasLastModifiedDate() bool {
	return n.Flags&FlagHasLastModifiedDate > 0
}
func (n *Needle) SetHasLastModifiedDate() {
	n.Flags = n.Flags | FlagHasLastModifiedDate
}
func (n *Needle) HasTtl() bool {
	return n.Flags&FlagHasTtl > 0
}
func (n *Needle) SetHasTtl() {
	n.Flags = n.Flags | FlagHasTtl
}

func (n *Needle) IsChunkedManifest() bool {
	return n.Flags&FlagIsChunkManifest > 0
}

func (n *Needle) SetIsChunkManifest() {
	n.Flags = n.Flags | FlagIsChunkManifest
}

func (n *Needle) HasPairs() bool {
	return n.Flags&FlagHasPairs != 0
}

func (n *Needle) SetHasPairs() {
	n.Flags = n.Flags | FlagHasPairs
}

func GetActualSize(size uint32, version Version) int64 {
	return NeedleHeaderSize + NeedleBodyLength(size, version)
}
