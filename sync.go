// Copyright (C) 2018-present Juicedata Inc.

package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juicedata/juicesync/object"
	"github.com/mattn/go-isatty"
)

// The max number of key per listing request
const MaxResults = 10240
const maxBlock = 10 << 20

var (
	found   uint64
	missing uint64
	copied  uint64
	failed  uint64
)

// Iterate on all the keys that starts at marker from object storage.
func Iterate(store object.ObjectStorage, marker, end string) (<-chan *object.Object, error) {
	objs, err := store.List("", marker, MaxResults)
	if err != nil {
		logger.Errorf("Can't list %s: %s", store, err.Error())
		return nil, err
	}
	out := make(chan *object.Object, MaxResults)
	go func() {
		lastkey := ""
	END:
		for len(objs) > 0 {
			for _, obj := range objs {
				key := obj.Key
				if key != "" && key <= lastkey {
					logger.Fatalf("The keys are out of order: %q >= %q", lastkey, key)
				}
				if end != "" && key >= end {
					break END
				}
				lastkey = key
				out <- obj
			}
			marker = lastkey
			objs, err = store.List("", marker, MaxResults)
			if err != nil {
				// Telling that the listing has failed
				out <- nil
				logger.Errorf("Fail to list after %s: %s", marker, err.Error())
				break
			}
		}
		close(out)
	}()
	return out, nil
}

func replicate(src, dst object.ObjectStorage, obj *object.Object) error {
	key := obj.Key
	firstBlock := -1
	if obj.Size > maxBlock {
		firstBlock = maxBlock
	}
	in, e := src.Get(key, 0, int64(firstBlock))
	if e != nil {
		if src.Exists(key) != nil {
			return nil
		}
		return e
	}
	data, err := ioutil.ReadAll(in)
	in.Close()
	if err != nil {
		return err
	}
	if firstBlock == -1 {
		return dst.Put(key, bytes.NewReader(data))
	}

	// download the object into disk first
	f, err := ioutil.TempFile("", "rep")
	if err != nil {
		return err
	}
	os.Remove(f.Name()) // will be deleted after Close()
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if in, e = src.Get(key, int64(len(data)), -1); e != nil {
		return e
	}
	_, e = io.Copy(f, in)
	in.Close()
	if e != nil {
		return e
	}
	// upload
	if _, e = f.Seek(0, 0); e != nil {
		return e
	}
	return dst.Put(key, f)
}

// sync comparing all the ordered keys from two object storage, replicate the missed ones.
func doSync(src, dst object.ObjectStorage, srckeys, dstkeys <-chan *object.Object) {
	todo := make(chan *object.Object, 1024)
	wg := sync.WaitGroup{}
	for i := 0; i < *threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				obj, ok := <-todo
				if !ok {
					break
				}
				start := time.Now()
				if err := replicate(src, dst, obj); err != nil {
					logger.Warningf("Failed to replicate %s from %s to %s: %s", obj.Key, src, dst, err.Error())
					atomic.AddUint64(&failed, 1)
				} else {
					atomic.AddUint64(&copied, 1)
				}
				logger.Debugf("copied %s in %s", obj.Key, time.Now().Sub(start))
			}
		}()
	}

	dstkey := ""
	hasMore := true
OUT:
	for obj := range srckeys {
		if obj == nil {
			logger.Errorf("Listing failed, stop replicating, waiting for pending ones")
			break
		}
		atomic.AddUint64(&found, 1)
		for hasMore && obj.Key > dstkey {
			dstobj, ok := <-dstkeys
			if !ok {
				hasMore = false
			} else if dstobj == nil {
				// Listing failed, stop
				logger.Errorf("Listing failed, stop replicating, waiting for pending ones")
				break OUT
			} else {
				dstkey = dstobj.Key
			}
		}
		if obj.Key < dstkey || !hasMore {
			todo <- obj
			atomic.AddUint64(&missing, 1)
		}
	}
	close(todo)
	wg.Wait()
}

func showProgress() {
	var lastCopied uint64
	var lastTime time.Time = time.Now()
	for {
		same := atomic.LoadUint64(&found) - atomic.LoadUint64(&missing)
		var width uint64 = 80
		a := width * same / found
		b := width * copied / found
		var bar [80]byte
		var i uint64
		for i = 0; i < width; i++ {
			if i < a {
				bar[i] = '='
			} else if i < a+b {
				bar[i] = '+'
			} else {
				bar[i] = ' '
			}
		}
		now := time.Now()
		fps := float64(copied-lastCopied) / now.Sub(lastTime).Seconds()
		lastCopied = copied
		lastTime = now
		fmt.Printf("[%s] %d%%  %.1f per second          \r", string(bar[:]), (found-missing+copied)*100/found, fps)
		time.Sleep(time.Millisecond * 300)
	}
}

// Sync syncs all the keys between to object storage
func Sync(src, dst object.ObjectStorage, marker, end string) error {
	logger.Infof("syncing between %s and %s (starting from %q)", src, dst, marker)
	cha, err := Iterate(src, marker, end)
	if err != nil {
		return err
	}
	chb, err := Iterate(dst, marker, end)
	if err != nil {
		return err
	}

	tty := isatty.IsTerminal(os.Stdout.Fd())
	if tty && !*verbose && !*quiet {
		go showProgress()
	}
	doSync(src, dst, cha, chb)
	logger.Infof("found: %d, copied: %d, failed: %d", atomic.LoadUint64(&found),
		atomic.LoadUint64(&copied), atomic.LoadUint64(&failed))
	return nil
}