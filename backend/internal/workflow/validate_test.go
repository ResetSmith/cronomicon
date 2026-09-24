package workflow_test

import (
	"testing"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// TestValidateSteps locks down the WB-S5 structural/enum rules at the unit level:
// type enums, the job/parallel/branch shape constraints, the branch condition
// enums, and the A12 upstream-input resolution (including that concurrent
// parallel siblings can't consume each other's outputs).
func TestValidateSteps(t *testing.T) {
	branch := func(cond *workflow.Condition, pass, fail *workflow.Branch) workflow.Step {
		return workflow.Step{Type: "branch", Condition: cond, Pass: pass, Fail: fail}
	}
	arm := func(steps ...workflow.Step) *workflow.Branch { return &workflow.Branch{Steps: steps} }
	job := func(name string) workflow.Step { return workflow.Step{Type: "job", Name: name} }

	cases := []struct {
		name      string
		steps     []workflow.Step
		wantValid bool
	}{
		{"empty graph", nil, true},
		{"linear jobs", []workflow.Step{job("a"), job("b")}, true},
		{"job missing name", []workflow.Step{{Type: "job"}}, false},
		{"unknown type", []workflow.Step{{Type: "frob", Name: "a"}}, false},
		{"empty type defaults to job", []workflow.Step{{Name: "a"}}, true},
		{"jobs[] on a job step", []workflow.Step{{Type: "job", Name: "a", Jobs: []workflow.Step{job("b")}}}, false},
		{"parallel ok", []workflow.Step{{Type: "parallel", Jobs: []workflow.Step{job("a"), job("b")}}}, true},
		{"parallel empty", []workflow.Step{{Type: "parallel"}}, false},
		{"parallel job unnamed", []workflow.Step{{Type: "parallel", Jobs: []workflow.Step{{Type: "job"}}}}, false},
		{
			"branch ok (job_status)",
			[]workflow.Step{job("a"), branch(&workflow.Condition{Type: "job_status", JobRef: "a"}, arm(job("b")), arm(job("c")))},
			true,
		},
		{
			"branch missing fail",
			[]workflow.Step{job("a"), {Type: "branch", Condition: &workflow.Condition{Type: "job_status", JobRef: "a"}, Pass: arm(job("b"))}},
			false,
		},
		{
			"branch missing condition",
			[]workflow.Step{{Type: "branch", Pass: arm(job("b")), Fail: arm(job("c"))}},
			false,
		},
		{
			"output_match ok",
			[]workflow.Step{job("a"), branch(&workflow.Condition{Type: "output_match", JobRef: "a", Field: "k", Operator: "=="}, arm(job("b")), arm(job("c")))},
			true,
		},
		{
			"output_match bad operator",
			[]workflow.Step{job("a"), branch(&workflow.Condition{Type: "output_match", JobRef: "a", Field: "k", Operator: ">="}, arm(job("b")), arm(job("c")))},
			false,
		},
		{
			"output_match missing field",
			[]workflow.Step{job("a"), branch(&workflow.Condition{Type: "output_match", JobRef: "a", Operator: "=="}, arm(job("b")), arm(job("c")))},
			false,
		},
		{
			"valid downstream input ref",
			[]workflow.Step{
				job("a"),
				{Type: "job", Name: "b", Inputs: map[string]workflow.InputRef{"V": {FromStep: "a", FromOutput: "v"}}},
			},
			true,
		},
		{
			"dangling input ref",
			[]workflow.Step{{Type: "job", Name: "a", Inputs: map[string]workflow.InputRef{"V": {FromStep: "ghost", FromOutput: "v"}}}},
			false,
		},
		{
			"parallel sibling input ref is not upstream",
			[]workflow.Step{{Type: "parallel", Jobs: []workflow.Step{
				job("a"),
				{Type: "job", Name: "b", Inputs: map[string]workflow.InputRef{"V": {FromStep: "a", FromOutput: "v"}}},
			}}},
			false,
		},
		{
			"input ref to a prior parallel block is upstream",
			[]workflow.Step{
				{Type: "parallel", Jobs: []workflow.Step{job("a"), job("b")}},
				{Type: "job", Name: "c", Inputs: map[string]workflow.InputRef{"V": {FromStep: "a", FromOutput: "v"}}},
			},
			true,
		},

		// ── PS-1: sequence arms inside parallel ─────────────────────────────────
		{
			"parallel arm may be a sequence",
			[]workflow.Step{{Type: "parallel", Jobs: []workflow.Step{
				job("solo"),
				{Type: "sequence", Steps: []workflow.Step{job("c1"), job("c2")}},
			}}},
			true,
		},
		{
			"sequence arm may nest a parallel and a branch",
			[]workflow.Step{
				job("seed"),
				{Type: "parallel", Jobs: []workflow.Step{
					{Type: "sequence", Steps: []workflow.Step{
						job("c1"),
						{Type: "parallel", Jobs: []workflow.Step{job("f1"), job("f2")}},
						branch(&workflow.Condition{Type: "job_status", JobRef: "c1"}, arm(job("ok")), arm(job("bad"))),
					}},
					job("solo"),
				}},
			},
			true,
		},
		{"empty sequence arm", []workflow.Step{{Type: "parallel", Jobs: []workflow.Step{{Type: "sequence"}}}}, false},
		{"empty standalone sequence", []workflow.Step{{Type: "sequence"}}, false},
		{"standalone sequence is an inline chain", []workflow.Step{{Type: "sequence", Steps: []workflow.Step{job("a"), job("b")}}}, true},
		{
			"branch directly as a parallel arm is still rejected",
			[]workflow.Step{{Type: "parallel", Jobs: []workflow.Step{
				branch(&workflow.Condition{Type: "job_status", JobRef: "x"}, arm(), arm()),
			}}},
			false,
		},
		{
			"steps[] on a job step",
			[]workflow.Step{{Type: "job", Name: "a", Steps: []workflow.Step{job("b")}}},
			false,
		},
		{
			"sequence arm steps see earlier steps of the SAME arm",
			[]workflow.Step{{Type: "parallel", Jobs: []workflow.Step{
				{Type: "sequence", Steps: []workflow.Step{
					job("producer"),
					{Type: "job", Name: "consumer", Inputs: map[string]workflow.InputRef{"V": {FromStep: "producer", FromOutput: "v"}}},
				}},
			}}},
			true,
		},
		{
			"sibling arms cannot see each other's producers",
			[]workflow.Step{{Type: "parallel", Jobs: []workflow.Step{
				{Type: "sequence", Steps: []workflow.Step{job("producer")}},
				{Type: "job", Name: "peeker", Inputs: map[string]workflow.InputRef{"V": {FromStep: "producer", FromOutput: "v"}}},
			}}},
			false,
		},
		{
			"a sequence arm's producers are upstream after the block",
			[]workflow.Step{
				{Type: "parallel", Jobs: []workflow.Step{
					{Type: "sequence", Steps: []workflow.Step{job("chain1"), job("chain2")}},
					job("solo"),
				}},
				{Type: "job", Name: "after", Inputs: map[string]workflow.InputRef{"V": {FromStep: "chain2", FromOutput: "v"}}},
			},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := workflow.ValidateSteps(tc.steps)
			gotValid := len(errs) == 0
			if gotValid != tc.wantValid {
				t.Errorf("ValidateSteps valid=%v (want %v); errors=%+v", gotValid, tc.wantValid, errs)
			}
		})
	}
}
