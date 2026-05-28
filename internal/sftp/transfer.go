package sftp

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/crypto/ssh"
)

func Upload(sshClient *ssh.Client, local, remote string, recursive, preserve bool) error {
	client, err := sftp.NewClient(sshClient)
	if err != nil {
		return fmt.Errorf("sftp: %w", err)
	}
	defer client.Close()

	info, err := os.Stat(local)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if !recursive {
			return fmt.Errorf("%s is a directory; use -r for recursive upload", local)
		}
		return uploadDir(client, local, remote, preserve)
	}
	return uploadFile(client, local, remote, info.Size(), preserve)
}

func uploadFile(client *sftp.Client, local, remote string, size int64, preserve bool) error {
	src, err := os.Open(local)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := client.Create(remote)
	if err != nil {
		return fmt.Errorf("create remote %s: %w", remote, err)
	}
	defer dst.Close()

	bar := progressbar.DefaultBytes(size, filepath.Base(local))
	_, err = io.Copy(io.MultiWriter(dst, bar), src)
	return err
}

func uploadDir(client *sftp.Client, local, remote string, preserve bool) error {
	return filepath.Walk(local, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(local, path)
		dst := remote + "/" + strings.ReplaceAll(rel, string(filepath.Separator), "/")

		if info.IsDir() {
			return client.MkdirAll(dst)
		}
		return uploadFile(client, path, dst, info.Size(), preserve)
	})
}

func Download(sshClient *ssh.Client, remote, local string, recursive, preserve bool) error {
	client, err := sftp.NewClient(sshClient)
	if err != nil {
		return fmt.Errorf("sftp: %w", err)
	}
	defer client.Close()

	info, err := client.Stat(remote)
	if err != nil {
		return fmt.Errorf("remote stat %s: %w", remote, err)
	}
	if info.IsDir() {
		if !recursive {
			return fmt.Errorf("%s is a directory; use -r for recursive download", remote)
		}
		return downloadDir(client, remote, local, preserve)
	}
	return downloadFile(client, remote, local, info.Size(), preserve)
}

func downloadFile(client *sftp.Client, remote, local string, size int64, preserve bool) error {
	src, err := client.Open(remote)
	if err != nil {
		return fmt.Errorf("open remote %s: %w", remote, err)
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(local), 0755); err != nil {
		return err
	}

	dst, err := os.Create(local)
	if err != nil {
		return err
	}
	defer dst.Close()

	bar := progressbar.DefaultBytes(size, filepath.Base(remote))
	_, err = io.Copy(io.MultiWriter(dst, bar), src)
	return err
}

func downloadDir(client *sftp.Client, remote, local string, preserve bool) error {
	walker := client.Walk(remote)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(remote, walker.Path())
		dst := filepath.Join(local, rel)

		if walker.Stat().IsDir() {
			if err := os.MkdirAll(dst, 0755); err != nil {
				return err
			}
			continue
		}
		if err := downloadFile(client, walker.Path(), dst, walker.Stat().Size(), preserve); err != nil {
			return err
		}
	}
	return nil
}
