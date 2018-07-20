package backend

import (
	"errors"
	"io"
	"io/ioutil"
	"sync"

	"code.uber.internal/infra/kraken/core"
	"code.uber.internal/infra/kraken/lib/backend/backenderrors"
)

// ManagerFixture returns a Manager with no clients for testing purposes.
func ManagerFixture() *Manager {
	m, err := NewManager(nil, AuthConfig{})
	if err != nil {
		panic(err)
	}
	return m
}

type testClient struct {
	sync.Mutex
	blobs map[string][]byte
}

// ClientFixture returns an in-memory Client for testing purposes.
func ClientFixture() Client {
	return &testClient{blobs: make(map[string][]byte)}
}

func (c *testClient) Stat(name string) (*core.BlobInfo, error) {
	c.Lock()
	defer c.Unlock()

	b, ok := c.blobs[name]
	if !ok {
		return nil, backenderrors.ErrBlobNotFound
	}
	return core.NewBlobInfo(int64(len(b))), nil
}

func (c *testClient) Upload(name string, src io.Reader) error {
	c.Lock()
	defer c.Unlock()

	b, err := ioutil.ReadAll(src)
	if err != nil {
		return err
	}
	c.blobs[name] = b
	return nil
}

func (c *testClient) Download(name string, dst io.Writer) error {
	c.Lock()
	defer c.Unlock()

	b, ok := c.blobs[name]
	if !ok {
		return backenderrors.ErrBlobNotFound
	}
	_, err := dst.Write(b)
	return err
}

func (c *testClient) List(dir string) ([]string, error) {
	return nil, errors.New("not supported")
}