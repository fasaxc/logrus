package logrus

import (
	"bytes"
	"sync"
)

var (
	bufferPool BufferPool
)

type BufferPool interface {
	Put(*bytes.Buffer)
	Get() *bytes.Buffer
}

type defaultPool struct {
	pool *sync.Pool
}

const maxCap = 1024*16

func (p *defaultPool) Put(buf *bytes.Buffer) {
	if buf.Cap() > maxCap {
		return
	}
	buf.Reset()
	p.pool.Put(buf)
}

func (p *defaultPool) Get() *bytes.Buffer {
	return p.pool.Get().(*bytes.Buffer)
}

// SetBufferPool allows to replace the default logrus buffer pool
// to better meets the specific needs of an application.
func SetBufferPool(bp BufferPool) {
	bufferPool = bp
}

func init() {
	SetBufferPool(&defaultPool{
		pool: &sync.Pool{
			New: func() interface{} {
				return new(bytes.Buffer)
			},
		},
	})
}
