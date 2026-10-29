package llm

import "sync"

// ItemEncoding belongs to one immutable projection of an item.
type ItemEncoding struct {
	once sync.Once
	raw  []byte
	err  error
}

func (e *ItemEncoding) Encode(f func() ([]byte, error)) ([]byte, error) {
	e.once.Do(func() { e.raw, e.err = f() })
	return e.raw, e.err
}
