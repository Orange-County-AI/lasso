package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"testing"

	"github.com/pkg/sftp"
)

func TestSFTPConnDead(t *testing.T) {
	dead := []error{
		sftp.ErrSSHFxConnectionLost,
		os.ErrClosed,
		io.EOF,
		io.ErrUnexpectedEOF,
		fmt.Errorf("write: failed to send packet: write |1: file already closed"),
		fmt.Errorf("failed to send packet: connection lost"),
		fmt.Errorf("write |1: broken pipe"),
		fmt.Errorf("wrapped: %w", os.ErrClosed),
	}
	for _, err := range dead {
		if !sftpConnDead(err) {
			t.Errorf("sftpConnDead(%v) = false, want true", err)
		}
	}
	alive := []error{
		nil,
		fs.ErrNotExist,
		errors.New("file does not exist"),
		errors.New("permission denied"),
		sftp.ErrSSHFxNoSuchFile,
	}
	for _, err := range alive {
		if sftpConnDead(err) {
			t.Errorf("sftpConnDead(%v) = true, want false", err)
		}
	}
}
