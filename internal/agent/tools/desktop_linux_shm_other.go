//go:build linux && !(amd64 || arm64)

package tools

import "errors"

type x11Shm struct{ ok bool }

func (b *x11Backend) shmRelease() {}

func (b *x11Backend) shmGetImage(x, y int16, w, h uint16) ([]byte, error) {
	return nil, errors.New("shm unsupported on this arch")
}
