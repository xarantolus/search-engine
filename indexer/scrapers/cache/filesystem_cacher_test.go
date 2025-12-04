package cache

import (
	"io"
	"os"
	"testing"
)

func TestCacher_Open(t *testing.T) {
	_ = os.RemoveAll("/tmp/cachetest")
	err := os.Mkdir("/tmp/cachetest", 0755)
	if err != nil {
		panic(err)
	}

	var cacher = NewCacher("/tmp/cachetest", "abcd", "/tmp")

	// Create file to be cached
	err = os.WriteFile("/tmp/myfile.txt", []byte("hello"), 0644)
	if err != nil {
		panic(err)
	}

	f, err := cacher.Open("myfile.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Check if the file is cached
	_, err = f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	// Read the entire file
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}

	err = f.Close()
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "hello" {
		t.Fatal("data mismatch")
	}

	// Remove the file
	err = os.Remove("/tmp/myfile.txt")
	if err != nil {
		panic(err)
	}
}
