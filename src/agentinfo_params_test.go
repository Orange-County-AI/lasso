package main

import (
	"reflect"
	"testing"
	"time"
)

// The output-side counterpart to create_params_test.go: together they close the
// loop, so a field can neither enter the create path nor the agent record
// without a recorded decision about whether MCP callers ever see it.

var (
	agentRecordType = reflect.TypeOf(AgentRecord{})
	agentInfoType   = reflect.TypeOf(agentInfo{})
)

func TestAgentInfoParamsDeclareEveryRecordField(t *testing.T) {
	for i := 0; i < agentRecordType.NumField(); i++ {
		f := agentRecordType.Field(i)
		p, ok := agentInfoParams[f.Name]
		if !ok {
			t.Errorf(`AgentRecord.%s is not declared in agentInfoParams (agentinfo_params.go).

Every persisted field must be a recorded decision about whether MCP callers can
see it, so a setting cannot become unreadable by accident. Add an entry, either:
  - surfacing it — add the field to agentInfo, copy it in agentInfoFrom, and
    record {Info: "<agentInfo field name>"}; or
  - keeping it server-side — record {Skip: "<why it stays out>"}.`, f.Name)
			continue
		}
		if (p.Info == "") == (p.Skip == "") {
			t.Errorf("agentInfoParams[%q] must set exactly one of Info (the agentInfo field that carries it) or Skip (why it is deliberately not surfaced)", f.Name)
		}
	}
	for name := range agentInfoParams {
		if _, ok := agentRecordType.FieldByName(name); !ok {
			t.Errorf("agentInfoParams has an entry for %q, which is not a field of AgentRecord — delete it (the field it guarded is gone)", name)
		}
	}
}

func TestAgentInfoFieldsAreAccountedFor(t *testing.T) {
	claimed := map[string]string{}
	for name, p := range agentInfoParams {
		if p.Info == "" {
			continue
		}
		if _, ok := agentInfoType.FieldByName(p.Info); !ok {
			t.Errorf("agentInfoParams[%q] says it surfaces as agentInfo.%s, but agentInfo has no such field — add it or change the entry to {Skip: \"<why>\"}", name, p.Info)
			continue
		}
		if prev, dup := claimed[p.Info]; dup {
			t.Errorf("agentInfo.%s is claimed by both agentInfoParams[%q] and agentInfoParams[%q]", p.Info, prev, name)
		}
		claimed[p.Info] = name
	}
	for i := 0; i < agentInfoType.NumField(); i++ {
		f := agentInfoType.Field(i)
		if _, ok := claimed[f.Name]; ok {
			continue
		}
		if why := agentInfoComputed[f.Name]; why != "" {
			continue
		}
		t.Errorf("agentInfo.%s is returned to MCP callers but nothing declares where it comes from — point an agentInfoParams entry at it, or record it in agentInfoComputed with its source.", f.Name)
	}
	for name := range agentInfoComputed {
		if _, ok := agentInfoType.FieldByName(name); !ok {
			t.Errorf("agentInfoComputed has an entry for %q, which is not a field of agentInfo — delete it", name)
		}
	}
}

// TestAgentInfoValuesReachTheCaller drives a fully-populated record through the
// real agentInfoFrom, so a field declared as surfaced but forgotten in that
// struct literal fails here rather than silently reading back empty.
func TestAgentInfoValuesReachTheCaller(t *testing.T) {
	derived := map[string]func(t *testing.T){
		"Host": func(t *testing.T) {
			// The host argument must win over rec.Host: a record found by the
			// cross-host whoami search has to name the host the caller can
			// address it on.
			got := agentInfoFrom("gigachad", AgentRecord{Host: "citadel"}, "")
			if got.Host != "gigachad" {
				t.Errorf("agentInfoFrom host = %q, want the host argument %q", got.Host, "gigachad")
			}
		},
		"CreatedAt": func(t *testing.T) {
			at := time.Date(2026, 7, 27, 5, 43, 29, 0, time.UTC)
			got := agentInfoFrom("local", AgentRecord{CreatedAt: at}, "")
			if want := at.Format(time.RFC3339); got.CreatedAt != want {
				t.Errorf("agentInfoFrom created_at = %q, want %q", got.CreatedAt, want)
			}
		},
	}

	probe := reflect.New(agentRecordType).Elem()
	for i := 0; i < agentRecordType.NumField(); i++ {
		f := agentRecordType.Field(i)
		v, err := probeValue(f)
		if err != nil {
			t.Fatalf("AgentRecord.%s: %v", f.Name, err)
		}
		probe.Field(i).Set(v)
	}
	rec := probe.Interface().(AgentRecord)
	// A probed BootStatus is not BootFailed, so surfacedStatus passes the live
	// herdr status through and Status stays out of this check's way.
	info := reflect.ValueOf(agentInfoFrom("local", rec, "idle"))

	for name, p := range agentInfoParams {
		if p.Info == "" {
			continue
		}
		if p.Derived != "" {
			check, ok := derived[name]
			if !ok {
				t.Errorf("agentInfoParams[%q] is Derived (%s) but has no case in this test's `derived` map — a transform nothing asserts can be silently broken. Add one.", name, p.Derived)
				continue
			}
			t.Run(name, check)
			continue
		}
		src, ok := agentRecordType.FieldByName(name)
		if !ok {
			continue // reported by TestAgentInfoParamsDeclareEveryRecordField
		}
		want, err := probeValue(src)
		if err != nil {
			continue
		}
		got := info.FieldByName(p.Info)
		if !got.IsValid() {
			continue // reported by TestAgentInfoFieldsAreAccountedFor
		}
		if got.Type() != want.Type() {
			t.Errorf("AgentRecord.%s is %s but agentInfo.%s is %s — the values cannot be copied across", name, want.Type(), p.Info, got.Type())
			continue
		}
		if !reflect.DeepEqual(got.Interface(), want.Interface()) {
			t.Errorf(`AgentRecord.%s never reaches agentInfo.%s: got %#v, want %#v.

The field is declared as surfaced but agentInfoFrom drops it, so every
get_agent/list_agents/whoami reads it back empty. Add `+"`%s: rec.%s,`"+` to
agentInfoFrom.`, name, p.Info, got.Interface(), want.Interface(), p.Info, name)
		}
	}
}

// A failed boot is terminal and must win over a zombie pane still reporting
// idle, so get_agent/list_agents show "failed" rather than a phantom healthy
// agent. BootStatus carries the precise phase alongside it.
func TestAgentInfoSurfacesFailedBootOverLiveStatus(t *testing.T) {
	rec := AgentRecord{BootStatus: BootFailed, BootError: "launch agent: boom"}
	got := agentInfoFrom("local", rec, "idle")
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed — a failed boot must beat the live herdr status", got.Status)
	}
	if got.BootStatus != BootFailed || got.BootError == "" {
		t.Errorf("boot_status = %q / boot_error = %q, want the failure and its reason surfaced", got.BootStatus, got.BootError)
	}
}
