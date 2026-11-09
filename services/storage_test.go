package services

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

const (
	accessDeniedXML = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>denied</Message></Error>`
	noSuchKeyXML    = `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`
)

// fakeS3 is an in-memory S3 endpoint covering the calls S3Storage makes.
type fakeS3 struct {
	mu          sync.Mutex
	buckets     map[string]bool
	objects     map[string][]byte
	contentType map[string]string
	denyAll     bool
	denyCreate  bool
}

func newFakeS3(t *testing.T) (*fakeS3, S3Config) {
	t.Helper()
	f := &fakeS3{buckets: map[string]bool{}, objects: map[string][]byte{}, contentType: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, S3Config{
		Endpoint:  strings.TrimPrefix(srv.URL, "http://"),
		AccessKey: "key",
		SecretKey: "secret",
		Bucket:    "bucket",
		Region:    "us-east-1",
	}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.denyAll {
		deny(w, r)
		return
	}

	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case key == "" && r.Method == http.MethodHead:
		if !f.buckets[bucket] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	case key == "" && r.Method == http.MethodPut:
		if f.denyCreate {
			deny(w, r)
			return
		}
		f.buckets[bucket] = true
		w.WriteHeader(http.StatusOK)
	case key == "" && r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		f.list(w, bucket, r.URL.Query().Get("prefix"))
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		body, ok := f.objects[bucket+"/"+key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			if r.Method == http.MethodGet {
				io.WriteString(w, noSuchKeyXML)
			}
			return
		}
		w.Header().Set("Content-Type", f.contentType[bucket+"/"+key])
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Write(body)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			body = decodeAWSChunked(body)
		}
		f.objects[bucket+"/"+key] = body
		f.contentType[bucket+"/"+key] = r.Header.Get("Content-Type")
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete:
		delete(f.objects, bucket+"/"+key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, bucket, prefix string) {
	type content struct{ Key string }
	result := struct {
		XMLName  xml.Name `xml:"ListBucketResult"`
		Name     string
		Prefix   string
		KeyCount int
		Contents []content
	}{Name: bucket, Prefix: prefix}
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, bucket+"/"+prefix) {
			keys = append(keys, strings.TrimPrefix(k, bucket+"/"))
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		result.Contents = append(result.Contents, content{k})
	}
	result.KeyCount = len(keys)
	w.Header().Set("Content-Type", "application/xml")
	xml.NewEncoder(w).Encode(result)
}

// decodeAWSChunked strips "<hex-size>;chunk-signature=...\r\n<data>\r\n" framing.
func decodeAWSChunked(framed []byte) []byte {
	var out bytes.Buffer
	r := bufio.NewReader(bytes.NewReader(framed))
	for {
		header, err := r.ReadString('\n')
		if err != nil {
			return out.Bytes()
		}
		sizeHex, _, _ := strings.Cut(strings.TrimSpace(header), ";")
		size, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil || size == 0 {
			return out.Bytes()
		}
		io.CopyN(&out, r, size)
		r.ReadString('\n')
	}
}

func deny(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusForbidden)
	if r.Method != http.MethodHead {
		io.WriteString(w, accessDeniedXML)
	}
}

func TestNewS3StorageCreatesMissingBucket(t *testing.T) {
	f, cfg := newFakeS3(t)

	if _, err := NewS3Storage(context.Background(), cfg); err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	if !f.buckets["bucket"] {
		t.Error("bucket not created")
	}
}

func TestNewS3StorageKeepsExistingBucket(t *testing.T) {
	f, cfg := newFakeS3(t)
	f.buckets["bucket"] = true
	f.denyCreate = true

	if _, err := NewS3Storage(context.Background(), cfg); err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
}

func TestNewS3StorageFailsWhenBucketCheckFails(t *testing.T) {
	f, cfg := newFakeS3(t)
	f.denyAll = true

	if _, err := NewS3Storage(context.Background(), cfg); err == nil {
		t.Fatal("expected error")
	}
}

func TestNewS3StorageFailsWhenBucketCreateFails(t *testing.T) {
	f, cfg := newFakeS3(t)
	f.denyCreate = true

	_, err := NewS3Storage(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "create bucket") {
		t.Fatalf("err = %v, want create bucket error", err)
	}
}

func TestNewS3StorageRejectsInvalidEndpoint(t *testing.T) {
	_, err := NewS3Storage(context.Background(), S3Config{Endpoint: "http://bad/endpoint", Bucket: "b"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestS3StorageUploadAndDelete(t *testing.T) {
	f, cfg := newFakeS3(t)
	s, err := NewS3Storage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}

	err = s.Upload(context.Background(), "u/e/obj", strings.NewReader("payload"), 7, "image/png")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if got := string(f.objects["bucket/u/e/obj"]); got != "payload" {
		t.Errorf("stored %q, want payload", got)
	}
	if got := f.contentType["bucket/u/e/obj"]; got != "image/png" {
		t.Errorf("content type %q, want image/png", got)
	}

	body, size, err := s.Download(context.Background(), "u/e/obj")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if string(got) != "payload" || size != 7 {
		t.Errorf("Download = %q (%d bytes), want payload (7)", got, size)
	}

	if err := s.Delete(context.Background(), "u/e/obj"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := f.objects["bucket/u/e/obj"]; ok {
		t.Error("object not deleted")
	}
}

func TestS3StorageUploadPropagatesError(t *testing.T) {
	f, cfg := newFakeS3(t)
	s, err := NewS3Storage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	f.denyAll = true

	if err := s.Upload(context.Background(), "k", strings.NewReader("x"), 1, "text/plain"); err == nil {
		t.Fatal("expected error")
	}
	if err := s.Delete(context.Background(), "k"); err == nil {
		t.Fatal("expected error")
	}
	if _, _, err := s.Download(context.Background(), "k"); err == nil {
		t.Fatal("expected error")
	}
	if err := s.DeletePrefix(context.Background(), "u/"); err == nil {
		t.Fatal("expected error")
	}
}

func TestS3StorageDownloadMissingObject(t *testing.T) {
	_, cfg := newFakeS3(t)
	s, err := NewS3Storage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}

	if _, _, err := s.Download(context.Background(), "missing"); err == nil {
		t.Fatal("expected error")
	}
}

func TestS3StorageDeletePrefix(t *testing.T) {
	f, cfg := newFakeS3(t)
	s, err := NewS3Storage(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	for _, k := range []string{"u1/e1/a", "u1/e2/b", "u2/e3/c"} {
		f.objects["bucket/"+k] = []byte("x")
	}

	if err := s.DeletePrefix(context.Background(), "u1/"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}

	if len(f.objects) != 1 || f.objects["bucket/u2/e3/c"] == nil {
		t.Errorf("remaining objects = %v, want only u2/e3/c", f.objects)
	}
}

func TestS3StorageIntegration(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := S3Config{
		Endpoint:  endpoint,
		AccessKey: os.Getenv("MINIO_TEST_ACCESS_KEY"),
		SecretKey: os.Getenv("MINIO_TEST_SECRET_KEY"),
		Bucket:    "it-" + time.Now().Format("20060102150405"),
		Region:    "us-east-1",
	}
	s, err := NewS3Storage(ctx, cfg)
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	t.Cleanup(func() { _ = s.client.RemoveBucket(context.Background(), cfg.Bucket) })

	if err := s.Upload(ctx, "user/entry/obj", strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	info, err := s.client.StatObject(ctx, cfg.Bucket, "user/entry/obj", minio.StatObjectOptions{})
	if err != nil {
		t.Fatalf("StatObject: %v", err)
	}
	if info.Size != 5 || info.ContentType != "text/plain" {
		t.Errorf("stat = size %d type %q", info.Size, info.ContentType)
	}

	body, size, err := s.Download(ctx, "user/entry/obj")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := io.ReadAll(body)
	body.Close()
	if string(got) != "hello" || size != 5 {
		t.Errorf("Download = %q (%d)", got, size)
	}

	if err := s.Delete(ctx, "user/entry/obj"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.client.StatObject(ctx, cfg.Bucket, "user/entry/obj", minio.StatObjectOptions{}); err == nil {
		t.Error("object still exists after Delete")
	}

	if err := s.EnsureBucket(ctx); err != nil {
		t.Errorf("EnsureBucket on existing bucket: %v", err)
	}

	for _, k := range []string{"a/1", "a/2", "b/1"} {
		if err := s.Upload(ctx, k, strings.NewReader("x"), 1, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeletePrefix(ctx, "a/"); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	var left []string
	for obj := range s.client.ListObjects(ctx, cfg.Bucket, minio.ListObjectsOptions{Recursive: true}) {
		left = append(left, obj.Key)
	}
	if len(left) != 1 || left[0] != "b/1" {
		t.Errorf("objects after DeletePrefix = %v, want [b/1]", left)
	}
	_ = s.Delete(ctx, "b/1")
}
