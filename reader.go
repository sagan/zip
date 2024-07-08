// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package zip

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

var (
	ErrFormat       = errors.New("zip: not a valid zip file")
	ErrAlgorithm    = errors.New("zip: unsupported compression algorithm")
	ErrChecksum     = errors.New("zip: checksum error")
	ErrInsecurePath = errors.New("zip: insecure file path")
)

type Reader struct {
	File    []*File
	Comment string

	name  string
	files []*os.File    // additional splitted zip files ("foo.z01", ... "foo.zXX", excluding "foo.zip").
	sizes []int64       // sizes of all splitted zip files (including foo.zip).
	rs    []io.ReaderAt // readers of all splitted zip files (including foo.zip).

	baseOffset int64
}

type ReadCloser struct {
	f *os.File
	Reader
}

type File struct {
	FileHeader
	zip          *Reader
	zipsize      int64
	headerOffset int64
}

func (f *File) hasDataDescriptor() bool {
	return f.Flags&0x8 != 0
}

// OpenReader will open the Zip file specified by name and return a ReadCloser.
//
// If any file inside the archive uses a non-local name
// (as defined by [filepath.IsLocal]) or a name containing backslashes
// and the GODEBUG environment variable contains `zipinsecurepath=0`,
// OpenReader returns the reader with an ErrInsecurePath error.
// A future version of Go may introduce this behavior by default.
// Programs that want to accept non-local names can ignore
// the ErrInsecurePath error and use the returned reader.
func OpenReader(name string) (*ReadCloser, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r := new(ReadCloser)
	r.name = name
	if err := r.init(f, fi.Size()); err != nil && err != ErrInsecurePath {
		f.Close()
		for _, f := range r.files {
			f.Close()
		}
		return nil, err
	}
	r.f = f
	return r, nil
}

// NewReader returns a new [Reader] reading from r, which is assumed to
// have the given size in bytes.
//
// If any file inside the archive uses a non-local name
// (as defined by [filepath.IsLocal]) or a name containing backslashes
// and the GODEBUG environment variable contains `zipinsecurepath=0`,
// NewReader returns the reader with an [ErrInsecurePath] error.
// A future version of Go may introduce this behavior by default.
// Programs that want to accept non-local names can ignore
// the [ErrInsecurePath] error and use the returned reader.
func NewReader(r io.ReaderAt, size int64) (*Reader, error) {
	zr := new(Reader)
	if err := zr.init(r, size); err != nil && err != ErrInsecurePath {
		return nil, err
	}
	return zr, nil
}

func (z *Reader) init(r io.ReaderAt, size int64) error {
	end, baseOffset, err := readDirectoryEnd(r, size)
	if err != nil {
		return err
	}
	if end.dirDiskNbr > 0 {
		if z.name == "" {
			return ErrFormat
		}
		ext := filepath.Ext(z.name)
		if ext != ".zip" {
			return ErrFormat
		}
		base := z.name[:len(z.name)-len(ext)]
		for i := uint32(0); i < end.dirDiskNbr; i++ {
			name := base + fmt.Sprintf(".z%02d", i+1)
			file, err := os.Open(name)
			if err != nil {
				return fmt.Errorf("failed to open split zip volume %q: %w", name, err)
			}
			fi, err := file.Stat()
			if err != nil {
				file.Close()
				return fmt.Errorf("failed to read split zip volume info %q: %w", name, err)
			}
			z.sizes = append(z.sizes, fi.Size())
			z.files = append(z.files, file)
			z.rs = append(z.rs, file)
		}
	}
	z.rs = append(z.rs, r)
	z.sizes = append(z.sizes, size)
	z.baseOffset = baseOffset

	totalSize := z.TotalSize()
	if end.directoryRecords > uint64(totalSize)/fileHeaderLen {
		return fmt.Errorf("archive/zip: TOC declares impossible %d files in %d byte zip", end.directoryRecords, size)
	}
	// r.File = make([]*File, 0, end.directoryRecords)
	// Since the number of directory records is not validated, it is not
	// safe to preallocate r.File without first checking that the specified
	// number of files is reasonable, since a malformed archive may
	// indicate it contains up to 1 << 128 - 1 files. Since each file has a
	// header which will be _at least_ 30 bytes we can safely preallocate
	// if (data size / 30) >= end.directoryRecords.
	if end.directorySize < uint64(totalSize) && (uint64(totalSize)-end.directorySize)/30 >= end.directoryRecords {
		z.File = make([]*File, 0, end.directoryRecords)
	}

	z.Comment = end.comment
	rs := io.NewSectionReader(z, 0, totalSize)
	directoryOffset := int64(0)
	for i := 0; i < int(end.dirDiskNbr); i++ {
		directoryOffset += z.sizes[i]
	}
	directoryOffset += +int64(end.directoryOffset)
	if _, err = rs.Seek(z.baseOffset+directoryOffset, io.SeekStart); err != nil {
		return err
	}
	buf := bufio.NewReader(rs)

	// The count of files inside a zip is truncated to fit in a uint16.
	// Gloss over this by reading headers until we encounter
	// a bad one, and then only report a ErrFormat or UnexpectedEOF if
	// the file count modulo 65536 is incorrect.
	for {
		f := &File{zip: z, zipsize: size}
		err = readDirectoryHeader(f, buf)
		if err == ErrFormat || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
		z.File = append(z.File, f)
	}
	if uint16(len(z.File)) != uint16(end.directoryRecords) { // only compare 16 bits here
		// Return the readDirectoryHeader error if we read
		// the wrong number of directory entries.
		return err
	}
	for _, f := range z.File {
		if f.Name == "" {
			// Zip permits an empty file name field.
			continue
		}
		if !filepath.IsLocal(f.Name) {
			return ErrInsecurePath
		}
	}
	return nil
}

// Total size of all splitted volumes
func (r *Reader) TotalSize() (size int64) {
	for _, s := range r.sizes {
		size += s
	}
	return size
}

// Close closes the Zip file, rendering it unusable for I/O.
func (rc *ReadCloser) Close() error {
	for _, f := range rc.files {
		f.Close()
	}
	return rc.f.Close()
}

func (r *Reader) ReadAt(p []byte, off int64) (n int, err error) {
	for i := 0; i < len(r.rs); i++ {
		remain := r.sizes[i] // remain bytes in current split zip volume
		if off > 0 {
			if off >= remain {
				off -= remain
				continue
			} else {
				remain -= off
			}
		}
		readlen := int(min(remain, int64(len(p)-n)))
		var readed int
		readed, err = r.rs[i].ReadAt(p[n:n+readlen], off)
		n += readed
		if readed != readlen {
			return n, err
		}
		if n == len(p) {
			break
		}
		off = 0
	}
	if n != len(p) {
		return n, ErrFormat
	}
	return
}

// Read the file in .zip from the file HeaderOffset. Supporting splitted zips.
func (f *File) ReadAt(p []byte, off int64) (n int, err error) {
	if int(f.DiskNbr) >= len(f.zip.rs) {
		return 0, ErrFormat
	}
	offset := int64(0)
	for i := uint16(0); i < f.DiskNbr; i++ {
		offset += f.zip.sizes[i]
	}
	offset += f.headerOffset + off
	n, err = f.zip.ReadAt(p, offset)
	return
}

// DataOffset returns the offset of the file's possibly-compressed
// data, relative to the beginning of the zip file.
//
// Most callers should instead use Open, which transparently
// decompresses data and verifies checksums.
func (f *File) DataOffset() (offset int64, err error) {
	bodyOffset, err := f.findBodyOffset()
	if err != nil {
		return
	}
	return f.headerOffset + bodyOffset, nil
}

// Open returns a ReadCloser that provides access to the File's contents.
// Multiple files may be read concurrently.
func (f *File) Open() (rc io.ReadCloser, err error) {
	bodyOffset, err := f.findBodyOffset()
	if err != nil {
		return
	}
	// If f is encrypted, CompressedSize64 includes salt, pwvv, encrypted data,
	// and auth code lengths
	size := int64(f.CompressedSize64)
	var r io.Reader
	rr := io.NewSectionReader(f, bodyOffset, size)
	// check for encryption
	if f.IsEncrypted() {
		if f.ae == 0 {
			if r, err = ZipCryptoDecryptor(rr, f.password()); err != nil {
				return
			}
		} else if r, err = newDecryptionReader(rr, f); err != nil {
			return
		}
	} else {
		r = rr
	}
	dcomp := decompressor(f.Method)
	if dcomp == nil {
		err = ErrAlgorithm
		return
	}
	rc = dcomp(r)
	// If AE-2, skip CRC and possible dataDescriptor
	if f.isAE2() {
		return
	}
	var desr io.Reader
	if f.hasDataDescriptor() {
		desr = io.NewSectionReader(f, bodyOffset+size, dataDescriptorLen)
	}
	rc = &checksumReader{
		rc:   rc,
		hash: crc32.NewIEEE(),
		f:    f,
		desr: desr,
	}
	return
}

// OpenRaw returns a [Reader] that provides access to the [File]'s contents without
// decompression.
func (f *File) OpenRaw() (io.Reader, error) {
	bodyOffset, err := f.findBodyOffset()
	if err != nil {
		return nil, err
	}
	r := io.NewSectionReader(f, bodyOffset, int64(f.CompressedSize64))
	return r, nil
}

type checksumReader struct {
	rc    io.ReadCloser
	hash  hash.Hash32
	nread uint64 // number of bytes read so far
	f     *File
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
	if err == nil {
		return
	}
	if err == io.EOF {
		if r.nread != r.f.UncompressedSize64 {
			return 0, io.ErrUnexpectedEOF
		}
		if r.desr != nil {
			if err1 := readDataDescriptor(r.desr, r.f); err1 != nil {
				if err1 == io.EOF {
					err = io.ErrUnexpectedEOF
				} else {
					err = err1
				}
			} else if r.hash.Sum32() != r.f.CRC32 {
				err = ErrChecksum
			}
		} else {
			// If there's not a data descriptor, we still compare
			// the CRC32 of what we've read against the file header
			// or TOC's CRC32, if it seems like it was set.
			if r.f.CRC32 != 0 && r.hash.Sum32() != r.f.CRC32 {
				err = ErrChecksum
			}
		}
	}
	r.err = err
	return
}

func (r *checksumReader) Close() error { return r.rc.Close() }

// findBodyOffset does the minimum work to verify the file has a header
// and returns the file body offset.
func (f *File) findBodyOffset() (int64, error) {
	var buf [fileHeaderLen]byte
	if _, err := f.ReadAt(buf[:], 0); err != nil {
		return 0, err
	}
	b := readBuf(buf[:])
	if sig := b.uint32(); sig != fileHeaderSignature {
		return 0, ErrFormat
	}
	b = b[22:] // skip over most of the header
	filenameLen := int(b.uint16())
	extraLen := int(b.uint16())
	return int64(fileHeaderLen + filenameLen + extraLen), nil
}

// readDirectoryHeader attempts to read a directory header from r.
// It returns io.ErrUnexpectedEOF if it cannot read a complete header,
// and ErrFormat if it doesn't find a valid header signature.
func readDirectoryHeader(f *File, r io.Reader) error {
	var buf [directoryHeaderLen]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return err
	}
	b := readBuf(buf[:])
	if sig := b.uint32(); sig != directoryHeaderSignature {
		return ErrFormat
	}
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
	f.DiskNbr = b.uint16()
	b = b[2:] // skipped internal attributes (uint16)
	f.ExternalAttrs = b.uint32()
	f.headerOffset = int64(b.uint32())
	d := make([]byte, filenameLen+extraLen+commentLen)
	if _, err := io.ReadFull(r, d); err != nil {
		return err
	}
	f.Name = string(d[:filenameLen])
	f.Extra = d[filenameLen : filenameLen+extraLen]
	f.Comment = string(d[filenameLen+extraLen:])

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

	if len(f.Extra) > 0 {
		b := readBuf(f.Extra)
		for len(b) >= 4 { // need at least tag and size
			tag := b.uint16()
			size := b.uint16()
			if int(size) > len(b) {
				return ErrFormat
			}
			eb := readBuf(b[:size])
			switch tag {
			case zip64ExtraId:
				// update directory values from the zip64 extra block
				if len(eb) >= 8 {
					f.UncompressedSize64 = eb.uint64()
				}
				if len(eb) >= 8 {
					f.CompressedSize64 = eb.uint64()
				}
				if len(eb) >= 8 {
					f.headerOffset = int64(eb.uint64())
				}
			case winzipAesExtraId:
				// grab the AE version
				f.ae = eb.uint16()
				// skip vendor ID
				_ = eb.uint16()
				// AES strength
				f.aesStrength = eb.uint8()
				// set the actual compression method.
				f.Method = eb.uint16()
			}
			b = b[size:]
		}
		// Should have consumed the whole header.
		// But popular zip & JAR creation tools are broken and
		// may pad extra zeros at the end, so accept those
		// too. See golang.org/issue/8186.
		for _, v := range b {
			if v != 0 {
				return ErrFormat
			}
		}
	}
	return nil
}

func readDataDescriptor(r io.Reader, f *File) error {
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
	if b.uint32() != f.CRC32 {
		return ErrChecksum
	}

	// The two sizes that follow here can be either 32 bits or 64 bits
	// but the spec is not very clear on this and different
	// interpretations has been made causing incompatibilities. We
	// already have the sizes from the central directory so we can
	// just ignore these.

	return nil
}

func readDirectoryEnd(r io.ReaderAt, size int64) (dir *directoryEnd, baseOffset int64, err error) {
	// look for directoryEndSignature in the last 1k, then in the last 65k
	var buf []byte
	var directoryEndOffset int64
	for i, bLen := range []int64{1024, 65 * 1024} {
		if bLen > size {
			bLen = size
		}
		buf = make([]byte, int(bLen))
		if _, err := r.ReadAt(buf, size-bLen); err != nil && err != io.EOF {
			return nil, 0, err
		}
		if p := findSignatureInBlock(buf); p >= 0 {
			buf = buf[p:]
			directoryEndOffset = size - bLen + int64(p)
			break
		}
		if i == 1 || bLen == size {
			return nil, 0, ErrFormat
		}
	}

	// read header into struct
	b := readBuf(buf[4:]) // skip signature
	d := &directoryEnd{
		diskNbr:            uint32(b.uint16()),
		dirDiskNbr:         uint32(b.uint16()),
		dirRecordsThisDisk: uint64(b.uint16()),
		directoryRecords:   uint64(b.uint16()),
		directorySize:      uint64(b.uint32()),
		directoryOffset:    uint64(b.uint32()),
		commentLen:         b.uint16(),
	}
	l := int(d.commentLen)
	if l > len(b) {
		return nil, 0, errors.New("zip: invalid comment length")
	}
	d.comment = string(b[:l])
	if d.dirDiskNbr > d.diskNbr {
		return d, 0, ErrFormat
	}

	// These values mean that the file can be a zip64 file
	if d.directoryRecords == 0xffff || d.directorySize == 0xffff || d.directoryOffset == 0xffffffff {
		p, err := findDirectory64End(r, directoryEndOffset)
		if err == nil && p >= 0 {
			directoryEndOffset = p
			err = readDirectory64End(r, p, d)
		}
		if err != nil {
			return nil, 0, err
		}
	}

	maxInt64 := uint64(1<<63 - 1)
	if d.directorySize > maxInt64 || d.directoryOffset > maxInt64 {
		return nil, 0, ErrFormat
	}

	if d.dirDiskNbr != d.diskNbr {
		return d, 0, nil
	}

	baseOffset = directoryEndOffset - int64(d.directorySize) - int64(d.directoryOffset)

	// Make sure directoryOffset points to somewhere in our file.
	if o := baseOffset + int64(d.directoryOffset); o < 0 || o >= size {
		return nil, 0, ErrFormat
	}

	// If the directory end data tells us to use a non-zero baseOffset,
	// but we would find a valid directory entry if we assume that the
	// baseOffset is 0, then just use a baseOffset of 0.
	// We've seen files in which the directory end data gives us
	// an incorrect baseOffset.
	if baseOffset > 0 {
		off := int64(d.directoryOffset)
		rs := io.NewSectionReader(r, off, size-off)
		if readDirectoryHeader(&File{}, rs) == nil {
			baseOffset = 0
		}
	}

	return d, baseOffset, nil
}

// findDirectory64End tries to read the zip64 locator just before the
// directory end and returns the offset of the zip64 directory end if
// found.
func findDirectory64End(r io.ReaderAt, directoryEndOffset int64) (int64, error) {
	locOffset := directoryEndOffset - directory64LocLen
	if locOffset < 0 {
		return -1, nil // no need to look for a header outside the file
	}
	buf := make([]byte, directory64LocLen)
	if _, err := r.ReadAt(buf, locOffset); err != nil {
		return -1, err
	}
	b := readBuf(buf)
	if sig := b.uint32(); sig != directory64LocSignature {
		return -1, nil
	}
	b = b[4:]       // skip number of the disk with the start of the zip64 end of central directory
	p := b.uint64() // relative offset of the zip64 end of central directory record
	return int64(p), nil
}

// readDirectory64End reads the zip64 directory end and updates the
// directory end with the zip64 directory end values.
func readDirectory64End(r io.ReaderAt, offset int64, d *directoryEnd) (err error) {
	buf := make([]byte, directory64EndLen)
	if _, err := r.ReadAt(buf, offset); err != nil {
		return err
	}

	b := readBuf(buf)
	if sig := b.uint32(); sig != directory64EndSignature {
		return ErrFormat
	}

	b = b[12:]                        // skip dir size, version and version needed (uint64 + 2x uint16)
	d.diskNbr = b.uint32()            // number of this disk
	d.dirDiskNbr = b.uint32()         // number of the disk with the start of the central directory
	d.dirRecordsThisDisk = b.uint64() // total number of entries in the central directory on this disk
	d.directoryRecords = b.uint64()   // total number of entries in the central directory
	d.directorySize = b.uint64()      // size of the central directory
	d.directoryOffset = b.uint64()    // offset of start of central directory with respect to the starting disk number

	return nil
}

func findSignatureInBlock(b []byte) int {
	for i := len(b) - directoryEndLen; i >= 0; i-- {
		// defined from directoryEndSignature in struct.go
		if b[i] == 'P' && b[i+1] == 'K' && b[i+2] == 0x05 && b[i+3] == 0x06 {
			// n is length of comment
			n := int(b[i+directoryEndLen-2]) | int(b[i+directoryEndLen-1])<<8
			if n+directoryEndLen+i <= len(b) {
				return i
			}
		}
	}
	return -1
}

type readBuf []byte

func (b *readBuf) uint8() byte {
	v := (*b)[0]
	*b = (*b)[1:]
	return v
}

func (b *readBuf) uint16() uint16 {
	v := binary.LittleEndian.Uint16(*b)
	*b = (*b)[2:]
	return v
}

func (b *readBuf) uint32() uint32 {
	v := binary.LittleEndian.Uint32(*b)
	*b = (*b)[4:]
	return v
}

func (b *readBuf) uint64() uint64 {
	v := binary.LittleEndian.Uint64(*b)
	*b = (*b)[8:]
	return v
}
