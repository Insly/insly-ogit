// Package testutil supplies an HTTP S3 contract fixture, not an AWS emulator.
package testutil

import (
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

type Object struct {
	Body       []byte
	Type, ETag string
}
type S3 struct {
	Server  *httptest.Server
	mu      sync.Mutex
	objects map[string]Object
	// Before runs before taking the object lock. Set only while requests are idle.
	Before func(http.ResponseWriter, *http.Request) bool
}

func NewS3(t testing.TB) *S3 {
	t.Helper()
	f := &S3{objects: map[string]Object{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}
func (f *S3) Client() *s3.Client {
	return s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), RetryMaxAttempts: 1}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(f.Server.URL)
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
}
func (f *S3) Get(key string) (Object, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[key]
	return o, ok
}
func (f *S3) Put(key string, body []byte, typ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = Object{append([]byte(nil), body...), typ, fmt.Sprintf("\"%x\"", sha256.Sum256(body))}
}
func (f *S3) Delete(key string) { f.mu.Lock(); defer f.mu.Unlock(); delete(f.objects, key) }
func (f *S3) serve(w http.ResponseWriter, r *http.Request) {
	if f.Before != nil && f.Before(w, r) {
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	f.mu.Lock()
	defer f.mu.Unlock()
	o, exists := f.objects[key]
	fail := func(code int, name string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(code)
		fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", name)
	}
	if r.URL.Query().Get("list-type") == "2" {
		type item struct{ Key string }
		result := struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			Contents    []item
			IsTruncated bool
		}{}
		prefix := key + "/" + r.URL.Query().Get("prefix")
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			result.Contents = append(result.Contents, item{strings.TrimPrefix(k, key+"/")})
		}
		_ = xml.NewEncoder(w).Encode(result)
		return
	}
	switch r.Method {
	case "GET", "HEAD":
		if !exists {
			fail(404, "NoSuchKey")
			return
		}
		w.Header().Set("ETag", o.ETag)
		w.Header().Set("Content-Length", fmt.Sprint(len(o.Body)))
		if o.Type != "" {
			w.Header().Set("X-Amz-Meta-Git-Type", o.Type)
		}
		if r.Method == "GET" {
			_, _ = w.Write(o.Body)
		}
	case "PUT":
		if (r.Header.Get("If-None-Match") == "*" && exists) || (r.Header.Get("If-Match") != "" && (!exists || r.Header.Get("If-Match") != o.ETag)) {
			fail(412, "PreconditionFailed")
			return
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			fail(400, "InvalidRequest")
			return
		}
		e := fmt.Sprintf("\"%x\"", sha256.Sum256(b))
		f.objects[key] = Object{b, r.Header.Get("X-Amz-Meta-Git-Type"), e}
		w.Header().Set("ETag", e)
	case "DELETE":
		delete(f.objects, key)
		w.WriteHeader(204)
	default:
		fail(405, "MethodNotAllowed")
	}
}
