package history

import (
	"fmt"
	"io"
	"sync"

	"github.com/go-git/go-git/v6/x/plugin"
	gitzlib "github.com/go-git/go-git/v6/x/plugin/zlib"
	kpzlib "github.com/klauspost/compress/zlib"
)

var zlibOnce sync.Once
var zlibErr error

func useKlauspostZlib() error {
	zlibOnce.Do(func() {
		zlibErr = plugin.Register(plugin.Zlib(), func() plugin.ZlibProvider {
			return klauspostZlibProvider{}
		})
	})
	return zlibErr
}

type klauspostZlibProvider struct{}

func (klauspostZlibProvider) NewReader(r io.Reader) (gitzlib.Reader, error) { //nolint:ireturn // plugin.ZlibProvider
	reader, err := kpzlib.NewReader(r)
	if err != nil {
		return nil, err
	}
	resettable, ok := reader.(gitzlib.Reader)
	if !ok {
		_ = reader.Close()
		return nil, fmt.Errorf("klauspost zlib reader %T does not implement reset", reader)
	}
	return resettable, nil
}

func (klauspostZlibProvider) NewWriter(w io.Writer) gitzlib.Writer { //nolint:ireturn // plugin.ZlibProvider
	return kpzlib.NewWriter(w)
}
