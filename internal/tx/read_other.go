//go:build !unix

package tx

import "os"

func openRead(path string) (*os.File, error) {
	return os.Open(path)
}

func openReadRoot(r *os.Root, name string) (*os.File, error) {
	return r.Open(name)
}
