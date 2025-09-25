package zipstream

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"sync"
	"time"
)

const (
	headerIdentifierLen = 4
	// fileHeaderLen            = 26
	dataDescriptorLen        = 16 // four uint32: descriptor signature, crc32, compressed size, size
	fileHeaderSignature      = 0x04034b50
	directoryHeaderSignature = 0x02014b50
	directoryEndSignature    = 0x06054b50
	dataDescriptorSignature  = 0x08074b50

	// Extra header IDs.
	// See http://mdfs.net/Docs/Comp/Archiving/Zip/ExtraField

	Zip64ExtraID       = 0x0001 // Zip64 extended information
	NtfsExtraID        = 0x000a // NTFS
	UnixExtraID        = 0x000d // UNIX
	ExtTimeExtraID     = 0x5455 // Extended timestamp
	InfoZipUnixExtraID = 0x5855 // Info-ZIP Unix extension

)

const (
	CompressMethodStored   = 0
	CompressMethodDeflated = 8
)

type Entry struct {
	zip.FileHeader
	r                          io.Reader
	lr                         io.Reader // LimitReader
	zip64                      bool
	hasReadNum                 uint64
	hasDataDescriptorSignature bool
	eof                        bool
	headerOffset               int64 // includes overall ZIP archive baseOffset

}

func (e *Entry) hasDataDescriptor() bool {
	return e.Flags&8 != 0
}

// IsDir just simply check whether the entry name ends with "/"
func (e *Entry) IsDir() bool {
	return len(e.Name) > 0 && e.Name[len(e.Name)-1] == '/'
}

func (e *Entry) Open() (io.ReadCloser, error) {
	if e.eof {
		return nil, errors.New("this file has read to end")
	}
	decomp := decompressor(e.Method)
	if decomp == nil {
		return nil, zip.ErrAlgorithm
	}
	rc := decomp(e.lr)

	return &checksumReader{
		rc:    rc,
		hash:  crc32.NewIEEE(),
		entry: e,
	}, nil
}

type Reader struct {
	r            io.Reader
	localFileEnd bool
	curEntry     *Entry
}

func NewReader(r io.Reader) *Reader {
	return &Reader{
		r: r,
	}
}

func (z *Reader) readEntry() (*Entry, error) {

	// buf := make([]byte, fileHeaderLen)
	// if _, err := io.ReadFull(z.r, buf); err != nil {
	// 	return nil, fmt.Errorf("unable to read local file header: %w", err)
	// }

	// lr := readBuf(buf)

	var buf [directoryHeaderLen]byte
	if _, err := io.ReadFull(z.r, buf[:]); err != nil {
		return nil, fmt.Errorf("unable to read local file header: %w", err)
	}
	lr := readBuf(buf[:])

	if sig := lr.uint32(); sig != directoryHeaderSignature {
		return nil, zip.ErrFormat
	}

	b := lr
	f := zip.FileHeader{}
	f.CreatorVersion = b.uint16()
	f.ReaderVersion = b.uint16()
	f.Flags = b.uint16()
	f.Method = b.uint16()
	f.ModifiedTime = b.uint16()
	f.ModifiedDate = b.uint16()
	f.CRC32 = b.uint32()
	f.CompressedSize = b.uint32()
	f.UncompressedSize = b.uint32()
	f.CompressedSize64 = uint64(f.CompressedSize)
	f.UncompressedSize64 = uint64(f.UncompressedSize)
	filenameLen := int(b.uint16())
	extraLen := int(b.uint16())
	commentLen := int(b.uint16())
	b = b[4:] // skipped start disk number and internal attributes (2x uint16)
	f.ExternalAttrs = b.uint32()

	headerOffset := int64(b.uint32())
	d := make([]byte, filenameLen+extraLen+commentLen)
	if _, err := io.ReadFull(z.r, d); err != nil {
		return nil, err
	}
	f.Name = string(d[:filenameLen])
	f.Extra = d[filenameLen : filenameLen+extraLen]
	f.Comment = string(d[filenameLen+extraLen:])

	// readerVersion := lr.uint16()
	// flags := lr.uint16()
	// method := lr.uint16()
	// modifiedTime := lr.uint16()
	// modifiedDate := lr.uint16()
	// crc32Sum := lr.uint32()
	// compressedSize := lr.uint32()
	// uncompressedSize := lr.uint32()
	// // filenameLen := int(lr.uint16())
	// extraAreaLen := int(lr.uint16())

	entry := &Entry{
		FileHeader:   f,
		r:            z.r,
		hasReadNum:   0,
		eof:          false,
		headerOffset: headerOffset,
	}

	nameAndExtraBuf := make([]byte, filenameLen+extraLen)
	if _, err := io.ReadFull(z.r, nameAndExtraBuf); err != nil {
		return nil, fmt.Errorf("unable to read entry name and extra area: %w", err)
	}

	// Determine the character encoding.
	utf8Valid1, utf8Require1 := detectUTF8(f.Name)
	utf8Valid2, utf8Require2 := detectUTF8(f.Comment)
	switch {
	case !utf8Valid1 || !utf8Valid2:
		// Name and Comment definitely not UTF-8.
		f.NonUTF8 = true
	case !utf8Require1 && !utf8Require2:
		// Name and Comment use only single-byte runes that overlap with UTF-8.
		f.NonUTF8 = false
	default:
		// Might be UTF-8, might be some other encoding; preserve existing flag.
		// Some ZIP writers use UTF-8 encoding without setting the UTF-8 flag.
		// Since it is impossible to always distinguish valid UTF-8 from some
		// other encoding (e.g., GBK or Shift-JIS), we trust the flag.
		f.NonUTF8 = f.Flags&0x800 == 0
	}

	// entry.Name = string(nameAndExtraBuf[:filenameLen])
	// entry.Extra = nameAndExtraBuf[filenameLen:]

	// entry.NonUTF8 = flags&0x800 == 0
	if f.Flags&1 == 1 {
		return nil, fmt.Errorf("encrypted ZIP entry not supported")
	}
	if f.Flags&8 == 8 && f.Method != CompressMethodDeflated {
		return nil, fmt.Errorf("only DEFLATED entries can have data descriptor")
	}

	needCSize := entry.CompressedSize == ^uint32(0)
	needUSize := entry.UncompressedSize == ^uint32(0)
	needHeaderOffset := entry.headerOffset == int64(^uint32(0))

	// ler := readBuf(entry.Extra)
	var modified time.Time
parseExtras:

	for extra := readBuf(f.Extra); len(extra) >= 4; { // need at least tag and size
		fieldTag := extra.uint16()
		fieldSize := int(extra.uint16())
		if len(extra) < fieldSize {
			break
		}
		fieldBuf := extra.sub(fieldSize)

		switch fieldTag {
		case zip64ExtraID:
			entry.zip64 = true

			// update directory values from the zip64 extra block.
			// They should only be consulted if the sizes read earlier
			// are maxed out.
			// See golang.org/issue/13367.
			if needUSize {
				needUSize = false
				if len(fieldBuf) < 8 {
					return nil, zip.ErrFormat
				}
				f.UncompressedSize64 = fieldBuf.uint64()
			}
			if needCSize {
				needCSize = false
				if len(fieldBuf) < 8 {
					return nil, zip.ErrFormat
				}
				f.CompressedSize64 = fieldBuf.uint64()
			}
			if needHeaderOffset {
				needHeaderOffset = false
				if len(fieldBuf) < 8 {
					return nil, zip.ErrFormat
				}
				entry.headerOffset = int64(fieldBuf.uint64())
			}
		case ntfsExtraID:
			if len(fieldBuf) < 4 {
				continue parseExtras
			}
			fieldBuf.uint32()        // reserved (ignored)
			for len(fieldBuf) >= 4 { // need at least tag and size
				attrTag := fieldBuf.uint16()
				attrSize := int(fieldBuf.uint16())
				if len(fieldBuf) < attrSize {
					continue parseExtras
				}
				attrBuf := fieldBuf.sub(attrSize)
				if attrTag != 1 || attrSize != 24 {
					continue // Ignore irrelevant attributes
				}

				const ticksPerSecond = 1e7    // Windows timestamp resolution
				ts := int64(attrBuf.uint64()) // ModTime since Windows epoch
				secs := ts / ticksPerSecond
				nsecs := (1e9 / ticksPerSecond) * (ts % ticksPerSecond)
				epoch := time.Date(1601, time.January, 1, 0, 0, 0, 0, time.UTC)
				modified = time.Unix(epoch.Unix()+secs, nsecs)
			}
		case unixExtraID, infoZipUnixExtraID:
			if len(fieldBuf) < 8 {
				continue parseExtras
			}
			fieldBuf.uint32()              // AcTime (ignored)
			ts := int64(fieldBuf.uint32()) // ModTime since Unix epoch
			modified = time.Unix(ts, 0)
		case extTimeExtraID:
			if len(fieldBuf) < 5 || fieldBuf.uint8()&1 == 0 {
				continue parseExtras
			}
			ts := int64(fieldBuf.uint32()) // ModTime since Unix epoch
			modified = time.Unix(ts, 0)
		}
	}

	// for len(ler) >= 4 { // need at least tag and size
	// 	fieldTag := ler.uint16()
	// 	fieldSize := int(ler.uint16())
	// 	if len(ler) < fieldSize {
	// 		break
	// 	}
	// 	fieldBuf := ler.sub(fieldSize)

	// 	switch fieldTag {
	// 	case Zip64ExtraID:
	// 		entry.zip64 = true

	// 		// update directory values from the zip64 extra block.
	// 		// They should only be consulted if the sizes read earlier
	// 		// are maxed out.
	// 		// See golang.org/issue/13367.
	// 		if needUSize {
	// 			needUSize = false
	// 			if len(fieldBuf) < 8 {
	// 				return nil, zip.ErrFormat
	// 			}
	// 			entry.UncompressedSize64 = fieldBuf.uint64()
	// 		}
	// 		if needCSize {
	// 			needCSize = false
	// 			if len(fieldBuf) < 8 {
	// 				return nil, zip.ErrFormat
	// 			}
	// 			entry.CompressedSize64 = fieldBuf.uint64()
	// 		}
	// 	case NtfsExtraID:
	// 		if len(fieldBuf) < 4 {
	// 			continue parseExtras
	// 		}
	// 		fieldBuf.uint32()        // reserved (ignored)
	// 		for len(fieldBuf) >= 4 { // need at least tag and size
	// 			attrTag := fieldBuf.uint16()
	// 			attrSize := int(fieldBuf.uint16())
	// 			if len(fieldBuf) < attrSize {
	// 				continue parseExtras
	// 			}
	// 			attrBuf := fieldBuf.sub(attrSize)
	// 			if attrTag != 1 || attrSize != 24 {
	// 				continue // Ignore irrelevant attributes
	// 			}

	// 			const ticksPerSecond = 1e7    // Windows timestamp resolution
	// 			ts := int64(attrBuf.uint64()) // ModTime since Windows epoch
	// 			secs := ts / ticksPerSecond
	// 			nsecs := (1e9 / ticksPerSecond) * int64(ts%ticksPerSecond)
	// 			epoch := time.Date(1601, time.January, 1, 0, 0, 0, 0, time.UTC)
	// 			modified = time.Unix(epoch.Unix()+secs, nsecs)
	// 		}
	// 	case UnixExtraID, InfoZipUnixExtraID:
	// 		if len(fieldBuf) < 8 {
	// 			continue parseExtras
	// 		}
	// 		fieldBuf.uint32()              // AcTime (ignored)
	// 		ts := int64(fieldBuf.uint32()) // ModTime since Unix epoch
	// 		modified = time.Unix(ts, 0)
	// 	case ExtTimeExtraID:
	// 		if len(fieldBuf) < 5 || fieldBuf.uint8()&1 == 0 {
	// 			continue parseExtras
	// 		}
	// 		ts := int64(fieldBuf.uint32()) // ModTime since Unix epoch
	// 		modified = time.Unix(ts, 0)
	// 	}
	// }

	msDosModified := MSDosTimeToTime(entry.ModifiedDate, entry.ModifiedTime)
	entry.Modified = msDosModified
	// msdosModified := msDosTimeToTime(f.ModifiedDate, f.ModifiedTime)
	// f.Modified = msdosModified

	if !modified.IsZero() {
		entry.Modified = modified.UTC()

		// If legacy MS-DOS timestamps are set, we can use the delta between
		// the legacy and extended versions to estimate timezone offset.
		//
		// A non-UTC timezone is always used (even if offset is zero).
		// Thus, FileHeader.Modified.Location() == time.UTC is useful for
		// determining whether extended timestamps are present.
		// This is necessary for users that need to do additional time
		// calculations when dealing with legacy ZIP formats.
		if entry.ModifiedTime != 0 || entry.ModifiedDate != 0 {
			entry.Modified = modified.In(timeZone(msDosModified.Sub(modified)))
		}
	}

	if needCSize {
		return nil, zip.ErrFormat
	}

	entry.lr = io.LimitReader(z.r, int64(entry.CompressedSize64))

	return entry, nil
}

func (z *Reader) GetNextEntry() (*Entry, error) {
	if z.localFileEnd {
		return nil, io.EOF
	}
	if z.curEntry != nil && !z.curEntry.eof {
		if z.curEntry.hasReadNum <= z.curEntry.UncompressedSize64 {
			if _, err := io.Copy(io.Discard, z.curEntry.lr); err != nil {
				return nil, fmt.Errorf("read previous file data fail: %w", err)
			}
			if z.curEntry.hasDataDescriptor() {
				if err := readDataDescriptor(z.r, z.curEntry); err != nil {
					return nil, fmt.Errorf("read previous entry's data descriptor fail: %w", err)
				}
			}
		} else {
			if !z.curEntry.hasDataDescriptor() {
				return nil, errors.New("parse error, read position exceed entry")
			}

			readDataLen := z.curEntry.hasReadNum - z.curEntry.UncompressedSize64
			if readDataLen > dataDescriptorLen {
				return nil, errors.New("parse error, read position exceed entry")
			} else if readDataLen > dataDescriptorLen-4 {
				if z.curEntry.hasDataDescriptorSignature {
					if _, err := io.Copy(io.Discard, io.LimitReader(z.r, int64(dataDescriptorLen-readDataLen))); err != nil {
						return nil, fmt.Errorf("read previous entry's data descriptor fail: %w", err)
					}
				} else {
					return nil, errors.New("parse error, read position exceed entry")
				}
			} else {
				buf := make([]byte, dataDescriptorLen-readDataLen)
				if _, err := io.ReadFull(z.r, buf); err != nil {
					return nil, fmt.Errorf("read previous entry's data descriptor fail: %w", err)
				}
				buf = buf[len(buf)-4:]
				headerID := binary.LittleEndian.Uint32(buf)

				// read to next record head
				if headerID == fileHeaderSignature ||
					headerID == directoryHeaderSignature ||
					headerID == directoryEndSignature {
					z.r = io.MultiReader(bytes.NewReader(buf), z.r)
				}
			}
		}
		z.curEntry.eof = true
	}
	headerIDBuf := make([]byte, headerIdentifierLen)
	if _, err := io.ReadFull(z.r, headerIDBuf); err != nil {
		return nil, fmt.Errorf("unable to read header identifier: %w", err)
	}
	headerID := binary.LittleEndian.Uint32(headerIDBuf)
	if headerID != fileHeaderSignature {
		if headerID == directoryHeaderSignature || headerID == directoryEndSignature {
			z.localFileEnd = true
			return nil, io.EOF
		}
		return nil, zip.ErrFormat
	}
	entry, err := z.readEntry()
	if err != nil {
		return nil, fmt.Errorf("unable to read zip file header: %w", err)
	}
	z.curEntry = entry
	return entry, nil
}

var (
	decompressors sync.Map // map[uint16]Decompressor
)

func init() {
	decompressors.Store(zip.Store, zip.Decompressor(io.NopCloser))
	decompressors.Store(zip.Deflate, zip.Decompressor(newFlateReader))
}

func decompressor(method uint16) zip.Decompressor {
	di, ok := decompressors.Load(method)
	if !ok {
		return nil
	}
	return di.(zip.Decompressor)
}

var flateReaderPool sync.Pool

func newFlateReader(r io.Reader) io.ReadCloser {
	fr, ok := flateReaderPool.Get().(io.ReadCloser)
	if ok {
		fr.(flate.Resetter).Reset(r, nil)
	} else {
		fr = flate.NewReader(r)
	}
	return &pooledFlateReader{fr: fr}
}

type pooledFlateReader struct {
	mu sync.Mutex // guards Close and Read
	fr io.ReadCloser
}

func (r *pooledFlateReader) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fr == nil {
		return 0, errors.New("Read after Close")
	}
	return r.fr.Read(p)
}

func (r *pooledFlateReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.fr != nil {
		err = r.fr.Close()
		flateReaderPool.Put(r.fr)
		r.fr = nil
	}
	return err
}

func readDataDescriptor(r io.Reader, entry *Entry) error {
	var buf [dataDescriptorLen]byte
	// The spec says: "Although not originally assigned a
	// signature, the value 0x08074b50 has commonly been adopted
	// as a signature value for the data descriptor record.
	// Implementers should be aware that ZIP files may be
	// encountered with or without this signature marking data
	// descriptors and should account for either case when reading
	// ZIP files to ensure compatibility."
	//
	// dataDescriptorLen includes the size of the signature but
	// first read just those 4 bytes to see if it exists.
	if _, err := io.ReadFull(r, buf[:4]); err != nil {
		return err
	}
	off := 0
	maybeSig := readBuf(buf[:4])
	if maybeSig.uint32() != dataDescriptorSignature {
		// No data descriptor signature. Keep these four
		// bytes.
		off += 4
	}
	if _, err := io.ReadFull(r, buf[off:12]); err != nil {
		return err
	}
	b := readBuf(buf[:12])
	if b.uint32() != entry.CRC32 {
		return zip.ErrChecksum
	}

	// The two sizes that follow here can be either 32 bits or 64 bits
	// but the spec is not very clear on this and different
	// interpretations has been made causing incompatibilities. We
	// already have the sizes from the central directory so we can
	// just ignore these.

	return nil
}

type File struct {
	FileHeader
	zip          *Reader
	zipr         io.ReaderAt
	headerOffset int64 // includes overall ZIP archive baseOffset
	zip64        bool  // zip64 extended information extra field presence
}

type checksumReader struct {
	rc    io.ReadCloser
	hash  hash.Hash32
	nread uint64 // number of bytes read so far
	// f     *File
	entry *Entry
	desr  io.Reader // if non-nil, where to read the data descriptor
	err   error     // sticky error
}

func (r *checksumReader) Read(b []byte) (n int, err error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err = r.rc.Read(b)
	r.hash.Write(b[:n])
	r.nread += uint64(n)
	if r.nread > r.entry.UncompressedSize64 {
		return 0, zip.ErrFormat
	}
	if err == nil {
		return
	}
	if err == io.EOF {
		if r.nread != r.entry.UncompressedSize64 {
			return 0, io.ErrUnexpectedEOF
		}
		if r.desr != nil {
			if err1 := readDataDescriptor(r.desr, r.entry); err1 != nil {
				if err1 == io.EOF {
					err = io.ErrUnexpectedEOF
				} else {
					err = err1
				}
			} else if r.hash.Sum32() != r.entry.CRC32 {
				err = zip.ErrChecksum
			}
		} else {
			// If there's not a data descriptor, we still compare
			// the CRC32 of what we've read against the file header
			// or TOC's CRC32, if it seems like it was set.
			if r.entry.CRC32 != 0 && r.hash.Sum32() != r.entry.CRC32 {
				err = zip.ErrChecksum
			}
		}
	}
	r.err = err
	return
}

func (r *checksumReader) Close() error { return r.rc.Close() }
