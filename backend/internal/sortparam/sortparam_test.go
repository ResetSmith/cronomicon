package sortparam

import (
	"net/url"
	"strings"
	"testing"
)

func vals(pairs ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v.Set(pairs[i], pairs[i+1])
	}
	return v
}

var allow = map[string]string{"name": "job_name", "status": RunStatusRank}

func TestOrderBy_DefaultWhenNoSort(t *testing.T) {
	got, err := OrderBy(vals(), allow, " ORDER BY created_at DESC", "id DESC")
	if err != nil || got != " ORDER BY created_at DESC" {
		t.Fatalf("got %q err=%v, want the default clause verbatim", got, err)
	}
	// An order without a sort key is inert, not an error (existing clients).
	got, err = OrderBy(vals("order", "desc"), allow, " ORDER BY created_at DESC", "")
	if err != nil || got != " ORDER BY created_at DESC" {
		t.Fatalf("order-without-sort: got %q err=%v", got, err)
	}
}

func TestOrderBy_BuildsClause(t *testing.T) {
	got, err := OrderBy(vals("sort", "name", "order", "desc"), allow, " ORDER BY created_at DESC", "created_at DESC, id DESC")
	if err != nil {
		t.Fatal(err)
	}
	want := " ORDER BY (job_name) IS NULL, job_name DESC, created_at DESC, id DESC"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// asc is the default direction.
	got, _ = OrderBy(vals("sort", "name"), allow, "", "")
	if !strings.HasSuffix(got, "job_name ASC") {
		t.Fatalf("got %q, want ASC default", got)
	}
}

func TestOrderBy_RankExpression(t *testing.T) {
	got, err := OrderBy(vals("sort", "status", "order", "asc"), allow, "", "id DESC")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "WHEN 'failure' THEN 0") || !strings.HasSuffix(got, "id DESC") {
		t.Fatalf("rank clause malformed: %q", got)
	}
}

func TestOrderBy_Rejections(t *testing.T) {
	if _, err := OrderBy(vals("sort", "job_name; DROP TABLE runs"), allow, "", ""); err == nil {
		t.Fatal("unknown sort key must be rejected, never interpolated")
	}
	if _, err := OrderBy(vals("sort", "name", "order", "sideways"), allow, "", ""); err == nil {
		t.Fatal("bad order value must be rejected")
	}
}
