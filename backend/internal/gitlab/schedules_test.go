package gitlab

import "testing"

// TestNormalizeSchedules covers the legacy/list resolution matrix and the
// per-entry validation rules wired into job/workflow YAML validation.
func TestNormalizeSchedules(t *testing.T) {
	cases := []struct {
		name        string
		legacy      string
		list        []ScheduleEntry
		wantEntries []ScheduleEntry // nil when an error is expected
		wantErr     bool
	}{
		{
			name:        "legacy cron becomes default entry",
			legacy:      "0 9 * * *",
			wantEntries: []ScheduleEntry{{Name: "default", Cron: "0 9 * * *"}},
		},
		{
			name:   "legacy Manual yields no entries",
			legacy: "Manual",
		},
		{
			name:   "legacy manual lowercase yields no entries",
			legacy: "manual",
		},
		{
			name:   "empty yields no entries",
			legacy: "",
		},
		{
			name:    "legacy bad cron errors",
			legacy:  "not a cron",
			wantErr: true,
		},
		{
			name:    "legacy and list are mutually exclusive",
			legacy:  "0 9 * * *",
			list:    []ScheduleEntry{{Name: "extra", Cron: "0 * * * *"}},
			wantErr: true,
		},
		{
			name: "valid list passes through",
			list: []ScheduleEntry{
				{Name: "nightly", Cron: "0 0 * * *"},
				{Name: "hourly", Cron: "0 * * * *", Env: map[string]string{"K": "v"}},
			},
			wantEntries: []ScheduleEntry{
				{Name: "nightly", Cron: "0 0 * * *"},
				{Name: "hourly", Cron: "0 * * * *", Env: map[string]string{"K": "v"}},
			},
		},
		{
			name:    "duplicate names (case-insensitive) error",
			list:    []ScheduleEntry{{Name: "nightly", Cron: "0 0 * * *"}, {Name: "Nightly", Cron: "0 1 * * *"}},
			wantErr: true,
		},
		{
			name:    "invalid entry name errors",
			list:    []ScheduleEntry{{Name: "Has Spaces", Cron: "0 0 * * *"}},
			wantErr: true,
		},
		{
			name:    "empty entry name errors",
			list:    []ScheduleEntry{{Name: "", Cron: "0 0 * * *"}},
			wantErr: true,
		},
		{
			name:    "bad cron in list errors",
			list:    []ScheduleEntry{{Name: "broken", Cron: "nope"}},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, errs := NormalizeSchedules(c.legacy, c.list)
			if c.wantErr {
				if len(errs) == 0 {
					t.Fatalf("expected validation errors, got none (entries=%v)", got)
				}
				return
			}
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if len(got) != len(c.wantEntries) {
				t.Fatalf("entries = %d, want %d (%v)", len(got), len(c.wantEntries), got)
			}
			for i := range got {
				if got[i].Name != c.wantEntries[i].Name || got[i].Cron != c.wantEntries[i].Cron {
					t.Errorf("entry[%d] = %+v, want %+v", i, got[i], c.wantEntries[i])
				}
			}
		})
	}
}
