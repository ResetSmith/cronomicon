package aws

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// AWS's published Signature Version 4 examples: the aws-sig-v4-test-suite's
// get-vanilla and post-vanilla, and the IAM ListUsers example of the
// "Signature Version 4 signing process" guide. Same example key and moment.
func TestSigV4PublishedVectors(t *testing.T) {
	cred := credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	for _, c := range []struct {
		name, method, url, region, service, want string
		headers                                  map[string]string
	}{
		{"get-vanilla", http.MethodGet, "https://example.amazonaws.com/", "us-east-1", "service",
			"5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31", nil},
		{"post-vanilla", http.MethodPost, "https://example.amazonaws.com/", "us-east-1", "service",
			"5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b", nil},
		{"iam-list-users", http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", "us-east-1", "iam",
			"5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7",
			map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=utf-8"}},
	} {
		req, _ := http.NewRequest(c.method, c.url, nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		sign(req, nil, cred, c.region, c.service, at)
		auth := req.Header.Get("Authorization")
		if !strings.HasSuffix(auth, "Signature="+c.want) {
			t.Errorf("%s: Authorization = %s\nwant signature %s", c.name, auth, c.want)
		}
	}
}

// A session credential signs its token as a header.
func TestSigV4SignsTheSessionToken(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://secretsmanager.eu-west-1.amazonaws.com/", nil)
	sign(req, []byte("{}"), credentials{AccessKeyID: "A", SecretAccessKey: "S", SessionToken: "tok"}, "eu-west-1", "secretsmanager", time.Now())
	if req.Header.Get("X-Amz-Security-Token") != "tok" || !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Fatalf("headers = %v", req.Header)
	}
	if req.Host != "secretsmanager.eu-west-1.amazonaws.com" {
		t.Fatalf("Host = %q", req.Host)
	}
}
