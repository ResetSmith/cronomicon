// Package fakes3 is an in-memory S3 endpoint for tests: just enough of the
// protocol for logarchive.Store — HEAD bucket, PUT/GET/HEAD/DELETE object,
// ListObjectsV2 — plus a fault switch so a test can make the service refuse.
//
// It exists because `make test` must not need a MinIO container or the
// network. It is a non-test package so the api and settings suites can use it
// too. It ignores request signatures entirely; what it does have to understand
// is the aws-chunked body encoding minio-go uses for a PUT over plain HTTP with
// V4 credentials (the streaming signature), or every uploaded object would
// carry the chunk framing as content.
package fakes3

import (
	"bufio"
	"bytes"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is one fake S3 service holding any number of buckets.
type Server struct {
	mu      sync.Mutex
	buckets map[string]map[string]object
	fail    *fault
	// Requests counts every request served, for tests that assert "no
	// ListObjects happened on GET" and the like.
	Requests []string

	ts *httptest.Server
}

type object struct {
	data     []byte
	modified time.Time
}

type fault struct {
	code   string
	status int
}

// New starts a plain-HTTP server with the given buckets pre-created. Close it
// with Close.
func New(buckets ...string) *Server {
	s := newServer(buckets)
	s.ts = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// NewTLS starts the same server over HTTPS with httptest's self-signed
// certificate — the "private S3 node with an internal CA" case. CertPEM hands
// back the certificate to paste as the CA bundle.
func NewTLS(buckets ...string) *Server {
	s := newServer(buckets)
	s.ts = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	return s
}

func newServer(buckets []string) *Server {
	s := &Server{buckets: map[string]map[string]object{}}
	for _, b := range buckets {
		s.buckets[b] = map[string]object{}
	}
	return s
}

// Endpoint is the host:port to hand to logarchive (no scheme; UseSSL per how
// the server was started).
func (s *Server) Endpoint() string {
	return strings.TrimPrefix(strings.TrimPrefix(s.ts.URL, "https://"), "http://")
}

// CertPEM is the TLS server's certificate as PEM (empty for a plain server).
func (s *Server) CertPEM() string {
	if s.ts.TLS == nil || len(s.ts.TLS.Certificates) == 0 || len(s.ts.TLS.Certificates[0].Certificate) == 0 {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.ts.TLS.Certificates[0].Certificate[0]}))
}

// Close stops the server.
func (s *Server) Close() { s.ts.Close() }

// Fail makes every subsequent request answer the given S3 error code and HTTP
// status (e.g. "AccessDenied", 403). Fail("", 0) clears it.
func (s *Server) Fail(code string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == "" {
		s.fail = nil
		return
	}
	s.fail = &fault{code: code, status: status}
}

// Put stores an object directly, bypassing HTTP (for seeding a test).
func (s *Server) Put(bucket, key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets[bucket] == nil {
		s.buckets[bucket] = map[string]object{}
	}
	s.buckets[bucket][key] = object{data: append([]byte(nil), data...), modified: time.Now().UTC()}
}

// Get returns an object's bytes, or nil,false.
func (s *Server) Get(bucket, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.buckets[bucket][key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), o.data...), true
}

// Keys returns the sorted keys in a bucket.
func (s *Server) Keys(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.buckets[bucket] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.Requests = append(s.Requests, r.Method+" "+r.URL.RequestURI())
	f := s.fail
	s.mu.Unlock()
	if f != nil {
		s3Error(w, f.status, f.code, "injected by fakes3")
		return
	}

	// Path-style: /{bucket} or /{bucket}/{key...}. minio-go uses path style for
	// any non-Amazon endpoint.
	p := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(p, "/")
	if bucket == "" {
		s3Error(w, http.StatusBadRequest, "InvalidRequest", "no bucket in path")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	objs, bucketOK := s.buckets[bucket]

	if key == "" {
		switch {
		case r.Method == http.MethodHead:
			if !bucketOK {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Query().Has("location"):
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
		case r.Method == http.MethodGet:
			if !bucketOK {
				s3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket "+bucket)
				return
			}
			s.list(w, r, bucket, objs)
		default:
			s3Error(w, http.StatusNotImplemented, "NotImplemented", r.Method+" bucket")
		}
		return
	}

	if !bucketOK {
		s3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket "+bucket)
		return
	}
	switch r.Method {
	case http.MethodPut:
		if r.URL.Query().Has("partNumber") || r.URL.Query().Has("uploadId") {
			s3Error(w, http.StatusNotImplemented, "NotImplemented", "multipart upload")
			return
		}
		body, err := readBody(r)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "IncompleteBody", err.Error())
			return
		}
		objs[key] = object{data: body, modified: time.Now().UTC()}
		w.Header().Set("ETag", `"`+strconv.Itoa(len(body))+`"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodHead, http.MethodGet:
		o, ok := objs[key]
		if !ok {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			s3Error(w, http.StatusNotFound, "NoSuchKey", "key "+key)
			return
		}
		data := o.data
		status := http.StatusOK
		w.Header().Set("Last-Modified", o.modified.Format(http.TimeFormat))
		w.Header().Set("ETag", `"`+strconv.Itoa(len(o.data))+`"`)
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Accept-Ranges", "bytes")
		if rng := r.Header.Get("Range"); rng != "" && r.Method == http.MethodGet {
			start, end, ok := parseRange(rng, int64(len(data)))
			if !ok {
				s3Error(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", rng)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			data = data[start : end+1]
			status = http.StatusPartialContent
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(status)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case http.MethodDelete:
		delete(objs, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusNotImplemented, "NotImplemented", r.Method+" object")
	}
}

type listResult struct {
	XMLName     xml.Name     `xml:"ListBucketResult"`
	Xmlns       string       `xml:"xmlns,attr"`
	Name        string       `xml:"Name"`
	Prefix      string       `xml:"Prefix"`
	KeyCount    int          `xml:"KeyCount"`
	MaxKeys     int          `xml:"MaxKeys"`
	IsTruncated bool         `xml:"IsTruncated"`
	Contents    []listObject `xml:"Contents"`
}

type listObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, bucket string, objs map[string]object) {
	prefix := r.URL.Query().Get("prefix")
	res := listResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Name: bucket, Prefix: prefix, MaxKeys: 1000}
	var keys []string
	for k := range objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		o := objs[k]
		res.Contents = append(res.Contents, listObject{
			Key: k, LastModified: o.modified.Format("2006-01-02T15:04:05.000Z"),
			ETag: `"` + strconv.Itoa(len(o.data)) + `"`, Size: int64(len(o.data)), StorageClass: "STANDARD",
		})
	}
	res.KeyCount = len(res.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(res)
}

func s3Error(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg)
}

// readBody returns the object bytes, decoding the aws-chunked framing when the
// request carries a streaming signature (minio-go's default for a PUT over
// plain HTTP with V4 credentials).
func readBody(r *http.Request) ([]byte, error) {
	sha := r.Header.Get("X-Amz-Content-Sha256")
	if !strings.HasPrefix(sha, "STREAMING-") && !strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		return io.ReadAll(r.Body)
	}
	return decodeAWSChunked(r.Body)
}

// decodeAWSChunked strips the `hex-size;chunk-signature=…\r\n<data>\r\n`
// framing. The terminal zero-size chunk may be followed by trailer headers,
// which are ignored.
func decodeAWSChunked(body io.Reader) ([]byte, error) {
	br := bufio.NewReader(body)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("aws-chunked: read size line: %w", err)
		}
		sizeHex, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ";")
		size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("aws-chunked: bad chunk size %q: %w", sizeHex, err)
		}
		if size == 0 {
			return out.Bytes(), nil
		}
		if _, err := io.CopyN(&out, br, size); err != nil {
			return nil, fmt.Errorf("aws-chunked: read chunk: %w", err)
		}
		// Consume the CRLF after the data.
		if _, err := br.ReadString('\n'); err != nil {
			return nil, fmt.Errorf("aws-chunked: read chunk terminator: %w", err)
		}
	}
}

// parseRange handles the one form minio's SetRange(offset, 0) emits: bytes=N-.
// A closed range bytes=N-M is accepted too.
func parseRange(h string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(spec, "-")
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end = size - 1
	if b != "" {
		if e, err := strconv.ParseInt(b, 10, 64); err == nil && e < end {
			end = e
		}
	}
	return start, end, true
}
