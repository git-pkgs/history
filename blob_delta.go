package history

import (
	"errors"
	"fmt"
	"math"

	packutil "github.com/go-git/go-git/v6/plumbing/format/packfile/util"
)

var errInvalidBlobDelta = errors.New("invalid blob delta")

const (
	minimumDeltaHeaderSize = 2
	bitsPerByte            = 8
)

func patchBlobDelta(source, delta []byte) ([]byte, error) {
	if len(delta) < minimumDeltaHeaderSize {
		return nil, errInvalidBlobDelta
	}
	sourceSize, delta, err := packutil.DecodeLEB128(delta)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidBlobDelta, err)
	}
	if sourceSize != uint(len(source)) {
		return nil, errInvalidBlobDelta
	}
	targetSize, delta, err := packutil.DecodeLEB128(delta)
	if err != nil || uint64(targetSize) > uint64(math.MaxInt) {
		return nil, fmt.Errorf("%w: target size", errInvalidBlobDelta)
	}
	result := make([]byte, int(targetSize))
	written := 0
	for written < len(result) {
		if len(delta) == 0 {
			return nil, errInvalidBlobDelta
		}
		command := delta[0]
		delta = delta[1:]
		if command&0x80 == 0 {
			size := int(command)
			if size == 0 || size > len(delta) || size > len(result)-written {
				return nil, errInvalidBlobDelta
			}
			copy(result[written:], delta[:size])
			written += size
			delta = delta[size:]
			continue
		}
		offset, size, rest, err := decodeBlobDeltaCopy(command, delta)
		if err != nil || offset > uint64(len(source)) || size > uint64(len(source))-offset || size > uint64(len(result)-written) {
			return nil, errInvalidBlobDelta
		}
		start, count := int(offset), int(size)
		copy(result[written:], source[start:start+count])
		written += count
		delta = rest
	}
	if len(delta) != 0 {
		return nil, errInvalidBlobDelta
	}
	return result, nil
}

func decodeBlobDeltaCopy(command byte, delta []byte) (offset, size uint64, rest []byte, err error) {
	read := func(bit byte, shift uint) error {
		if command&bit == 0 {
			return nil
		}
		if len(delta) == 0 {
			return errInvalidBlobDelta
		}
		offset |= uint64(delta[0]) << shift
		delta = delta[1:]
		return nil
	}
	for index, bit := range []byte{0x01, 0x02, 0x04, 0x08} {
		if err := read(bit, uint(index*bitsPerByte)); err != nil {
			return 0, 0, nil, err
		}
	}
	for index, bit := range []byte{0x10, 0x20, 0x40} {
		if command&bit == 0 {
			continue
		}
		if len(delta) == 0 {
			return 0, 0, nil, errInvalidBlobDelta
		}
		size |= uint64(delta[0]) << uint(index*bitsPerByte)
		delta = delta[1:]
	}
	if size == 0 {
		size = 0x10000
	}
	return offset, size, delta, nil
}
