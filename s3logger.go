// Program s3logger collects json messages over TCP, stores them into
// gzip-compressed files split by time and uploads these files to AWS s3 bucket.
//
// Once started, service accepts TCP connections and expects clients to send
// streams of json objects over such connections. s3logger only closes
// connection if it encounters malformed json or single object size exceeds
// 4 MiB limit. s3logger only reads data from the client.
//
// s3logger accumulates received messages over predefined time window (-t flag)
// to a temporary log file creating new ones as needed; previous files are
// uploaded to s3 bucket in background and removed after successful upload.
// Optionally maximum size of input read can be specified in megabytes (-mb
// flag) to rotate file before reaching predefined time.  Program only writes to
// a single temporary log file at a time, so json messages received from
// multiple concurrent connections are interleaved into a single json stream. It
// does its best not to lose messages, but can still drop them if they're coming
// faster than could be saved on disk or there's any disk write error. Stored
// messages are separated by new line (0xa).
//
// s3logger uploads files to a specified bucket using predefined s3 object
// naming scheme:
//
// 	dt=2018-02-09/20180209T213803_df718a7818e53243.json.gz
//
// It uses dt=YYYY-MM-DD "directories", object name base starting with date
// and time when log file was created (UTC) followed by hex-encoded 64-bit
// random value and .json.gz suffix.
//
// See
// https://godoc.org/github.com/aws/aws-sdk-go/aws/session#hdr-Environment_Variables
// on how to configure s3 bucket access credentials.
//
// s3logger does not use TLS for its listener at the moment as it is expected to
// run on localhost or inside trusted network.
//
//	Usage of s3logger:
//	  -addr string
//		address to listen (default "localhost:8080")
//	  -bucket string
//		s3 bucket to upload logs
//	  -dir string
//		directory to keep unsent files (default "/var/spool/s3logger")
//	  -mb int
//		megabytes of input read until file is rotated (0 to disable) (default 512)
//	  -prefix string
//		s3 object name prefix (directory in a bucket)
//	  -t duration
//		time to use single file (min 1m) (default 5m0s)
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/artyom/autoflags"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"golang.org/x/sync/errgroup"
)

func main() {
	args := runArgs{
		Dir:  "/var/spool/s3logger",
		D:    5 * time.Minute,
		Mb:   512,
		Addr: "localhost:8080",
	}
	autoflags.Parse(&args)
	log := log.New(os.Stderr, "", log.LstdFlags)
	sess, err := session.NewSession()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)
		log.Print(<-sigCh)
		cancel()
	}()
	if err := run(ctx, args, log, s3manager.NewUploader(sess)); err != nil {
		log.Fatal(err)
	}
}

type runArgs struct {
	Addr   string        `flag:"addr,address to listen"`
	Bucket string        `flag:"bucket,s3 bucket to upload logs"`
	Dir    string        `flag:"dir,directory to keep unsent files"`
	Prefix string        `flag:"prefix,s3 object name prefix (directory in a bucket)"`
	D      time.Duration `flag:"t,time to use single file (min 1m)"`
	Mb     int           `flag:"mb,megabytes of input read until file is rotated (0 to disable)"`
}

func run(ctx context.Context, args runArgs, logger *log.Logger, upl *s3manager.Uploader) error {
	if args.Addr == "" {
		return errors.New("empty address")
	}
	if args.Bucket == "" {
		return errors.New("empty bucket name")
	}
	if args.Dir == "" {
		args.Dir = "."
	}
	if args.D < time.Minute {
		args.D = time.Minute
	}
	if err := os.MkdirAll(args.Dir, 0777); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", args.Addr)
	if err != nil {
		return err
	}
	defer ln.Close()

	srv := &server{dir: args.Dir, ch: make(chan json.RawMessage, 1000), wake: make(chan struct{}), log: logger}
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { <-ctx.Done(); return ln.Close() })
	group.Go(func() error { return srv.ingest(ctx, args.Mb<<20, args.D) })
	group.Go(func() error { return srv.upload(ctx, args.D/2, args.Bucket, args.Prefix, upl) })
	group.Go(func() error {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return nil
				default:
				}
				return err
			}
			go func(conn net.Conn) {
				if err := srv.handleConn(ctx, conn); err != nil {
					logger.Printf("%s: %v", conn.RemoteAddr(), err)
				}
			}(conn)
		}
	})
	return group.Wait()
}

type server struct {
	dir  string
	ch   chan json.RawMessage
	wake chan struct{} // to wake uploading goroutine early
	log  *log.Logger

	mu   sync.Mutex
	w    io.WriteCloser
	name string
}

func (srv *server) upload(ctx context.Context, d time.Duration, bucket, prefix string, upl *s3manager.Uploader) error {
	walkFn := func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".json.gz") || path == srv.currentName() {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		start := time.Now()
		switch err := uploadFile(ctx, upl, bucket, prefix, path); err {
		case nil:
			srv.log.Printf("%q uploaded in %v", path, time.Since(start).Round(100*time.Millisecond))
			_ = os.Remove(path)
		default:
			srv.log.Printf("%q upload: %v", path, err)
		}
		return nil
	}
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = filepath.Walk(srv.dir, walkFn)
		case <-srv.wake:
			_ = filepath.Walk(srv.dir, walkFn)
		}
	}
}

func uploadFile(ctx context.Context, upl *s3manager.Uploader, bucket, prefix, name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	key := path.Join(prefix, strings.Replace(filepath.Base(name), ".", "/", 1))
	_, err = upl.UploadWithContext(ctx, &s3manager.UploadInput{Bucket: &bucket, Key: &key, Body: f})
	return err
}

func (srv *server) ingest(ctx context.Context, maxSize int, d time.Duration) error {
	var enc *json.Encoder
	var size int
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case msg := <-srv.ch:
			if enc == nil {
				switch w, err := srv.create(); err {
				case nil:
					enc = json.NewEncoder(w)
					size = 0
				default:
					srv.log.Print("file create: ", err)
					continue
				}
			}
			if err := enc.Encode(msg); err != nil {
				srv.log.Print("message write: ", err)
				srv.close()
				enc = nil
			}
			size += len(msg)
			if maxSize > 0 && size >= maxSize {
				timer.Reset(d)
				srv.close()
				enc = nil
				select {
				case srv.wake <- struct{}{}:
				default:
				}
			}
		case <-timer.C:
			timer.Reset(d)
			srv.close()
			enc = nil
			select {
			case srv.wake <- struct{}{}:
			default:
			}
		case <-ctx.Done():
			return srv.close()
		}
	}
}

func (srv *server) create() (io.Writer, error) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	f, err := os.Create(filepath.Join(srv.dir, randomName()+".json.gz"))
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(f, 1<<16)
	gw, err := gzip.NewWriterLevel(bw, gzip.BestSpeed)
	if err != nil {
		panic(err)
	}
	srv.name, srv.w = f.Name(), chain{gw, bw, f}
	return srv.w, nil
}

func (srv *server) close() error {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.w == nil {
		return nil
	}
	w := srv.w
	srv.name, srv.w = "", nil
	return w.Close()
}

func (srv *server) currentName() string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.name
}

func (srv *server) handleConn(ctx context.Context, conn io.ReadCloser) error {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(3 * time.Minute)
	}
	const maxSize = 4 << 20 // single json object size limit (approximate)
	rd := &io.LimitedReader{R: bufio.NewReader(conn), N: maxSize}
	dec := json.NewDecoder(rd)
	for {
		var msg json.RawMessage
		switch err := dec.Decode(&msg); err {
		case nil:
			rd.N = maxSize
		case io.EOF:
			return nil
		default:
			return err
		}
		select {
		case srv.ch <- msg:
		case <-ctx.Done():
			return nil
		}
	}
}

// chain implements io.WriteCloser that passes writes to the first (0 index)
// Writer and on Close flushes and closes each Writer if they implement relevant
// interfaces.
type chain []io.Writer

func (cw chain) Write(p []byte) (int, error) { return cw[0].Write(p) }
func (cw chain) Close() error {
	var errOut error
	for _, w := range cw {
		if f, ok := w.(interface{ Flush() error }); ok {
			if err := f.Flush(); err != nil && errOut == nil {
				errOut = err
			}
		}
		if c, ok := w.(io.Closer); ok {
			if err := c.Close(); err != nil && errOut == nil {
				errOut = err
			}
		}
	}
	return errOut
}

// randomName returns base name of the temporary file. It encodes file creation
// date and can be translated to s3 object name in date-sharded "subdirectories"
// by replacing . with /.
func randomName() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return time.Now().In(time.UTC).Format("dt=2006-01-02.20060102T150405_") + hex.EncodeToString(b)
}
