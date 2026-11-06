// Package fakeaws is a stand-in for AWS Secrets Manager's GetSecretValue, for
// tests outside the aws package (as logarchive/fakes3 is for S3). It answers
// over TLS, refuses a request signed with another access key id as AWS
// refuses an unknown one, and serves the secrets it was given. The aws
// package's own tests verify signatures; this one does not.
package fakeaws

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Server is a running fake.
type Server struct {
	*httptest.Server
	mu      sync.Mutex
	keyID   string
	secrets map[string]string
	Reads   int
}

// New starts a fake that accepts keyID and holds secrets (SecretId →
// SecretString). Close it when done.
func New(keyID string, secrets map[string]string) *Server {
	s := &Server{keyID: keyID, secrets: secrets}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	return s
}

// CAPEM is the fake's certificate, for a connection's CA bundle.
func (s *Server) CAPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
}

// SetKeyID changes the access key id the fake accepts.
func (s *Server) SetKeyID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keyID = id
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Reads++
	fail := func(code int, typ, msg string) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"__type": typ, "message": msg})
	}
	if r.Header.Get("X-Amz-Target") != "secretsmanager.GetSecretValue" {
		fail(http.StatusBadRequest, "UnknownOperationException", "")
		return
	}
	if !strings.Contains(r.Header.Get("Authorization"), "Credential="+s.keyID+"/") {
		fail(http.StatusBadRequest, "UnrecognizedClientException", "The security token included in the request is invalid.")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var in struct{ SecretId string }
	_ = json.Unmarshal(body, &in)
	v, ok := s.secrets[in.SecretId]
	if !ok {
		fail(http.StatusBadRequest, "ResourceNotFoundException", "Secrets Manager can't find the specified secret.")
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"Name": in.SecretId, "SecretString": v})
}
