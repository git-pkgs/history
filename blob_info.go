package history

import (
	"bufio"
	"crypto"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"strings"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/format/idxfile"
	gogithash "github.com/go-git/go-git/v6/plumbing/hash"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/klauspost/compress/zlib"
)

type blobObjectInfo struct {
	hash    plumbing.Hash
	size    int64
	storage *filesystem.Storage
	pack    *blobPack
	packed  *packedObjectInfo
}

const (
	estimatedBlobFraction = 2
	packTypeShift         = 4
	packTypeMask          = 7
	packInitialSizeMask   = 15
	packPayloadMask       = 0x7f
	packPayloadBits       = 7
	packContinuationMask  = 0x80
)

type blobPack struct {
	fs       billy.Filesystem
	path     string
	index    idxfile.Index
	idSize   int
	storage  *filesystem.Storage
	byOffset map[int64]*packedObjectInfo
	objects  []*packedObjectInfo
}

type packedObjectInfo struct {
	blobObjectInfo
	offset     int64
	rawType    plumbing.ObjectType
	baseOffset int64
	baseHash   plumbing.Hash
	base       *packedObjectInfo
	content    int64
	deltaSize  int64
	resolved   plumbing.ObjectType
	resolving  bool
}

type countingReader struct {
	reader *bufio.Reader
	read   int64
}

func (r *countingReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	r.read += int64(n)
	return n, err
}

func (r *countingReader) ReadByte() (byte, error) {
	value, err := r.reader.ReadByte()
	if err == nil {
		r.read++
	}
	return value, err
}

type singleByteReader struct {
	reader io.Reader
	buffer [1]byte
}

func (r *singleByteReader) ReadByte() (byte, error) {
	_, err := io.ReadFull(r.reader, r.buffer[:])
	return r.buffer[0], err
}

type blobObjectReader struct {
	packs   map[*blobPack]*blobPackReader
	decoded map[string][]byte
}

type blobPackReader struct {
	file     billy.File
	inflater io.ReadCloser
	stream   readerAtCursor
}

type readerAtCursor struct {
	reader io.ReaderAt
	offset int64
	buffer [1]byte
}

func (r *readerAtCursor) reset(reader io.ReaderAt, offset int64) {
	r.reader = reader
	r.offset = offset
}

func (r *readerAtCursor) Read(data []byte) (int, error) {
	n, err := r.reader.ReadAt(data, r.offset)
	r.offset += int64(n)
	return n, err
}

func (r *readerAtCursor) ReadByte() (byte, error) {
	_, err := r.Read(r.buffer[:])
	return r.buffer[0], err
}

func newBlobObjectReader() *blobObjectReader {
	return &blobObjectReader{
		packs:   make(map[*blobPack]*blobPackReader),
		decoded: make(map[string][]byte),
	}
}

func (r *blobObjectReader) read(info blobObjectInfo) ([]byte, error) {
	if info.pack == nil {
		encoded, err := info.storage.EncodedObject(plumbing.BlobObject, info.hash)
		if err != nil {
			return nil, err
		}
		return readBlob(encoded)
	}
	return r.readPacked(info.packed)
}

func (r *blobObjectReader) readPacked(info *packedObjectInfo) ([]byte, error) {
	key := info.hash.String()
	if data := r.decoded[key]; data != nil {
		return data, nil
	}
	if !info.rawType.IsDelta() {
		data, err := r.inflate(info, info.size)
		if err != nil {
			return nil, err
		}
		r.decoded[key] = data
		return data, nil
	}
	delta, err := r.inflate(info, info.deltaSize)
	if err != nil {
		return nil, err
	}
	var base []byte
	if info.base != nil {
		base, err = r.readPacked(info.base)
	} else {
		encoded, readErr := info.storage.EncodedObject(plumbing.BlobObject, info.baseHash)
		if readErr != nil {
			return nil, readErr
		}
		base, err = readBlob(encoded)
	}
	if err != nil {
		return nil, err
	}
	data, err := patchBlobDelta(base, delta)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != info.size {
		return nil, fmt.Errorf("object %s size changed during enumeration: %d != %d", info.hash, len(data), info.size)
	}
	r.decoded[key] = data
	return data, nil
}

func (r *blobObjectReader) inflate(info *packedObjectInfo, size int64) ([]byte, error) {
	if size < 0 || uint64(size) > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("object %s size %d cannot be represented in memory", info.hash, size)
	}
	packed := r.packs[info.pack]
	if packed == nil {
		file, err := info.pack.fs.Open(info.pack.path)
		if err != nil {
			return nil, err
		}
		packed = &blobPackReader{file: file}
		r.packs[info.pack] = packed
	}
	packed.stream.reset(packed.file, info.content)
	var err error
	if packed.inflater == nil {
		packed.inflater, err = zlib.NewReader(&packed.stream)
	} else {
		err = packed.inflater.(zlib.Resetter).Reset(&packed.stream, nil)
	}
	if err != nil {
		return nil, err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(packed.inflater, data); err != nil {
		return nil, err
	}
	var extra [1]byte
	if n, err := packed.inflater.Read(extra[:]); err != io.EOF || n != 0 {
		if err == nil {
			err = errors.New("inflated object exceeds declared size")
		}
		return nil, err
	}
	return data, nil
}

func (r *blobObjectReader) clear() {
	r.decoded = make(map[string][]byte)
}

func (r *blobObjectReader) close() error {
	var result error
	for key, packed := range r.packs {
		if packed.inflater != nil {
			result = errors.Join(result, packed.inflater.Close())
		}
		result = errors.Join(result, packed.file.Close())
		delete(r.packs, key)
	}
	return result
}

func blobObjectInfos(stores []*filesystem.Storage, objectFormat formatcfg.ObjectFormat) ([]blobObjectInfo, error) {
	packed, allPacks, byHash, err := packedBlobObjects(stores, objectFormat)
	if err != nil {
		return nil, err
	}
	for _, info := range packed {
		if _, err := resolvePackedObjectType(info, byHash); err != nil {
			return nil, err
		}
	}
	for _, pack := range allPacks {
		if err := readPackedBlobSizes(pack); err != nil {
			return nil, err
		}
	}
	result, seen := uniquePackedBlobInfos(packed)
	return appendLooseBlobInfos(result, seen, stores, objectFormat)
}

func packedBlobObjects(
	stores []*filesystem.Storage,
	objectFormat formatcfg.ObjectFormat,
) ([]*packedObjectInfo, []*blobPack, map[string]*packedObjectInfo, error) {
	var packed []*packedObjectInfo
	var allPacks []*blobPack
	byHash := make(map[string]*packedObjectInfo)
	for _, store := range stores {
		storePacks, err := readBlobPacks(store, objectFormat)
		if err != nil {
			return nil, nil, nil, err
		}
		allPacks = append(allPacks, storePacks...)
		for _, pack := range storePacks {
			infos, err := readPackedObjectInfos(pack)
			if err != nil {
				return nil, nil, nil, err
			}
			packed = append(packed, infos...)
			for _, info := range infos {
				key := info.hash.String()
				current := byHash[key]
				if current == nil || current.rawType.IsDelta() && !info.rawType.IsDelta() {
					byHash[key] = info
				}
			}
		}
	}
	return packed, allPacks, byHash, nil
}

func uniquePackedBlobInfos(packed []*packedObjectInfo) ([]blobObjectInfo, map[string]struct{}) {
	seen := make(map[string]struct{})
	result := make([]blobObjectInfo, 0, len(packed)/estimatedBlobFraction)
	for _, info := range packed {
		if info.resolved != plumbing.BlobObject {
			continue
		}
		key := info.hash.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, info.blobObjectInfo)
	}
	return result, seen
}

func appendLooseBlobInfos(
	result []blobObjectInfo,
	seen map[string]struct{},
	stores []*filesystem.Storage,
	objectFormat formatcfg.ObjectFormat,
) ([]blobObjectInfo, error) {
	for _, store := range stores {
		loose, err := looseBlobObjectInfos(store, objectFormat)
		if err != nil {
			return nil, err
		}
		for _, info := range loose {
			key := info.hash.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, info)
		}
	}
	return result, nil
}

func readBlobPacks(store *filesystem.Storage, objectFormat formatcfg.ObjectFormat) ([]*blobPack, error) {
	fs := store.Filesystem()
	entries, err := fs.ReadDir(fs.Join("objects", "pack"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	idSize := objectFormat.Size()
	if idSize == 0 {
		idSize = crypto.SHA1.Size()
	}
	var result []*blobPack
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".idx") {
			continue
		}
		base := strings.TrimSuffix(entry.Name(), ".idx")
		indexPath := fs.Join("objects", "pack", entry.Name())
		packPath := fs.Join("objects", "pack", base+".pack")
		if _, err := fs.Stat(packPath); err != nil {
			return nil, err
		}
		index, err := readPackIndex(fs, indexPath, idSize)
		if err != nil {
			return nil, err
		}
		result = append(result, &blobPack{
			fs: fs, path: packPath, index: index, idSize: idSize,
			storage: store, byOffset: make(map[int64]*packedObjectInfo),
		})
	}
	return result, nil
}

func readPackIndex(fs billy.Filesystem, path string, idSize int) (_ *idxfile.MemoryIndex, resultErr error) {
	file, err := fs.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := file.Close(); resultErr == nil {
			resultErr = err
		}
	}()
	index := idxfile.NewMemoryIndex(idSize)
	if err := idxfile.NewDecoder(file, objectIDHasher(idSize)).Decode(index); err != nil {
		return nil, err
	}
	return index, nil
}

func objectIDHasher(idSize int) hash.Hash {
	if idSize == crypto.SHA256.Size() {
		return gogithash.New(crypto.SHA256)
	}
	return gogithash.New(crypto.SHA1)
}

func readPackedObjectInfos(pack *blobPack) (_ []*packedObjectInfo, resultErr error) {
	file, err := pack.fs.Open(pack.path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := file.Close(); resultErr == nil {
			resultErr = err
		}
	}()
	entries, err := pack.index.EntriesByOffset()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := entries.Close(); resultErr == nil {
			resultErr = err
		}
	}()
	reader := bufio.NewReaderSize(file, readerBufferSize)
	var result []*packedObjectInfo
	for {
		entry, err := entries.Next()
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		if entry.Offset > math.MaxInt64 {
			return nil, fmt.Errorf("pack offset %d overflows int64", entry.Offset)
		}
		offset := int64(entry.Offset)
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
		reader.Reset(file)
		info, err := readPackedObjectInfo(reader, offset, pack.idSize)
		if err != nil {
			return nil, fmt.Errorf("object %s at offset %d: %w", entry.Hash, offset, err)
		}
		hash, ok := plumbing.FromHex(entry.Hash.String())
		if !ok {
			return nil, fmt.Errorf("invalid object ID %q", entry.Hash)
		}
		info.hash = hash
		info.storage = pack.storage
		info.pack = pack
		info.packed = info
		info.offset = offset
		pack.byOffset[offset] = info
		pack.objects = append(pack.objects, info)
		result = append(result, info)
	}
}

func readPackedObjectInfo(reader *bufio.Reader, offset int64, idSize int) (*packedObjectInfo, error) {
	counted := &countingReader{reader: reader}
	first, err := counted.ReadByte()
	if err != nil {
		return nil, err
	}
	typ := plumbing.ObjectType((first >> packTypeShift) & packTypeMask)
	if !typ.Valid() {
		return nil, fmt.Errorf("invalid object type %d", typ)
	}
	size := uint64(first & packInitialSizeMask)
	shift := uint(packTypeShift)
	for first&packContinuationMask != 0 {
		first, err = counted.ReadByte()
		if err != nil {
			return nil, err
		}
		value := uint64(first & packPayloadMask)
		if shift >= 64 || value > math.MaxUint64>>shift {
			return nil, errors.New("object size overflows uint64")
		}
		size |= value << shift
		shift += 7
	}
	if size > math.MaxInt64 {
		return nil, errors.New("object size overflows int64")
	}
	info := &packedObjectInfo{rawType: typ}
	switch typ {
	case plumbing.OFSDeltaObject:
		distance, err := readOffsetDeltaDistance(counted)
		if err != nil {
			return nil, err
		}
		if distance > uint64(offset) {
			return nil, errors.New("offset delta base precedes pack")
		}
		info.baseOffset = offset - int64(distance)
	case plumbing.REFDeltaObject:
		encoded := make([]byte, idSize)
		if _, err := io.ReadFull(counted, encoded); err != nil {
			return nil, err
		}
		base, ok := plumbing.FromBytes(encoded)
		if !ok {
			return nil, errors.New("invalid reference delta object ID")
		}
		info.baseHash = base
	default:
		info.size = int64(size)
		info.content = offset + counted.read
		return info, nil
	}
	info.content = offset + counted.read
	info.deltaSize = int64(size)
	return info, nil
}

func readPackedBlobSizes(pack *blobPack) (resultErr error) {
	file, err := pack.fs.Open(pack.path)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	reader := bufio.NewReaderSize(file, readerBufferSize)
	var inflater io.ReadCloser
	defer func() {
		if inflater != nil {
			resultErr = errors.Join(resultErr, inflater.Close())
		}
	}()
	deltaReader := &singleByteReader{}
	for _, info := range pack.objects {
		if info.resolved != plumbing.BlobObject || !info.rawType.IsDelta() {
			continue
		}
		if _, err := file.Seek(info.content, io.SeekStart); err != nil {
			return err
		}
		reader.Reset(file)
		if inflater == nil {
			inflater, err = zlib.NewReader(reader)
		} else {
			err = inflater.(zlib.Resetter).Reset(reader, nil)
		}
		if err != nil {
			return err
		}
		deltaReader.reader = inflater
		if _, err := readDeltaSize(deltaReader); err != nil {
			return err
		}
		targetSize, err := readDeltaSize(deltaReader)
		if err != nil {
			return err
		}
		info.size = targetSize
	}
	return nil
}

func readOffsetDeltaDistance(reader io.ByteReader) (uint64, error) {
	current, err := reader.ReadByte()
	if err != nil {
		return 0, err
	}
	distance := uint64(current & packPayloadMask)
	for current&packContinuationMask != 0 {
		current, err = reader.ReadByte()
		if err != nil {
			return 0, err
		}
		if distance > (math.MaxUint64>>packPayloadBits)-1 {
			return 0, errors.New("offset delta distance overflows uint64")
		}
		distance = ((distance + 1) << packPayloadBits) | uint64(current&packPayloadMask)
	}
	return distance, nil
}

func readDeltaSize(reader io.ByteReader) (int64, error) {
	var size uint64
	for shift := uint(0); ; shift += packPayloadBits {
		current, err := reader.ReadByte()
		if err != nil {
			return 0, err
		}
		value := uint64(current & packPayloadMask)
		if shift >= 64 || value > math.MaxUint64>>shift {
			return 0, errors.New("delta size overflows uint64")
		}
		size |= value << shift
		if current&packContinuationMask == 0 {
			if size > math.MaxInt64 {
				return 0, errors.New("delta size overflows int64")
			}
			return int64(size), nil
		}
	}
}

func resolvePackedObjectType(
	info *packedObjectInfo,
	byHash map[string]*packedObjectInfo,
) (plumbing.ObjectType, error) {
	if info.resolved != plumbing.InvalidObject {
		return info.resolved, nil
	}
	if !info.rawType.IsDelta() {
		info.resolved = info.rawType
		return info.resolved, nil
	}
	if info.resolving {
		return plumbing.InvalidObject, errors.New("delta object cycle")
	}
	info.resolving = true
	defer func() { info.resolving = false }()

	var base *packedObjectInfo
	if info.rawType == plumbing.OFSDeltaObject {
		base = info.pack.byOffset[info.baseOffset]
	} else {
		base = byHash[info.baseHash.String()]
	}
	if base != nil {
		info.base = base
		typ, err := resolvePackedObjectType(base, byHash)
		if err != nil {
			return plumbing.InvalidObject, err
		}
		info.resolved = typ
		return typ, nil
	}
	if info.baseHash.IsZero() {
		return plumbing.InvalidObject, errors.New("delta base not found")
	}
	encoded, err := info.storage.EncodedObject(plumbing.AnyObject, info.baseHash)
	if err != nil {
		return plumbing.InvalidObject, err
	}
	info.resolved = encoded.Type()
	return info.resolved, nil
}

func looseBlobObjectInfos(
	store *filesystem.Storage,
	objectFormat formatcfg.ObjectFormat,
) ([]blobObjectInfo, error) {
	fs := store.Filesystem()
	entries, err := fs.ReadDir("objects")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	idSize := objectFormat.Size()
	if idSize == 0 {
		idSize = crypto.SHA1.Size()
	}
	var result []blobObjectInfo
	for _, directory := range entries {
		if !directory.IsDir() || len(directory.Name()) != 2 || !hexString(directory.Name()) {
			continue
		}
		children, err := fs.ReadDir(fs.Join("objects", directory.Name()))
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			encoded := directory.Name() + child.Name()
			if child.IsDir() || len(encoded) != idSize*2 || !hexString(child.Name()) {
				continue
			}
			hash, ok := plumbing.FromHex(encoded)
			if !ok {
				continue
			}
			object, err := store.EncodedObject(plumbing.AnyObject, hash)
			if err != nil {
				return nil, err
			}
			if object.Type() == plumbing.BlobObject {
				result = append(result, blobObjectInfo{
					hash: hash, size: object.Size(), storage: store,
				})
			}
		}
	}
	return result, nil
}

func hexString(value string) bool {
	for i := range len(value) {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}
