package main

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// These tests are the enforcement half of createParams (see create_params.go
// for why the parity contract is a registry rather than one shared struct).
// Between them they make an *undeclared* create-payload field impossible:
// adding a field to createAgentReq without either exposing it downstream or
// recording why it isn't fails here, naming the field and both ways to fix it.

var (
	createReqType = reflect.TypeOf(createAgentReq{})
	createInType  = reflect.TypeOf(createAgentIn{})
)

// jsonName is the wire name of a struct field, minus the ",omitempty" tail.
func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	return strings.Split(tag, ",")[0]
}

// TestCreateParamsDeclareEveryField is the backstop the Effort drift needed:
// every field of the create payload must be a recorded decision.
func TestCreateParamsDeclareEveryField(t *testing.T) {
	for i := 0; i < createReqType.NumField(); i++ {
		f := createReqType.Field(i)
		p, ok := createParams[f.Name]
		if !ok {
			t.Errorf(`createAgentReq.%s is not declared in createParams (create_params.go).

Every field of the create payload must be a deliberate, recorded decision, so
it cannot silently fail to reach a caller. Add an entry for %q, either:
  - exposing it over MCP — add the field to createAgentIn with a jsonschema
    description, copy it in createAgentIn.toCreateReq, and record
    {MCP: "<createAgentIn field name>"}; or
  - opting out on purpose — record {MCPSkip: "<why MCP must not have it>"}.
Do the same for the browser payload with TS/TSSkip.`, f.Name, f.Name)
			continue
		}
		if (p.MCP == "") == (p.MCPSkip == "") {
			t.Errorf("createParams[%q] must set exactly one of MCP (the createAgentIn field that carries it) or MCPSkip (why it is deliberately not exposed over MCP)", f.Name)
		}
		if (p.TS == "") == (p.TSSkip == "") {
			t.Errorf("createParams[%q] must set exactly one of TS (the CreateAgentPayload property) or TSSkip (why the browser payload leaves it out)", f.Name)
		}
		if p.MCP != "" && p.MCPDerived == "" && p.MCP != f.Name {
			// A rename with no transform is almost always a typo; a genuine
			// rename is a transform of sorts and should say so.
			t.Errorf("createParams[%q] maps to createAgentIn.%s under a different name without explaining why — set MCPDerived to record the reason", f.Name, p.MCP)
		}
	}
	// A stale entry outlives the field it described and quietly stops guarding
	// anything, so treat it as a failure too.
	for name := range createParams {
		if _, ok := createReqType.FieldByName(name); !ok {
			t.Errorf("createParams has an entry for %q, which is not a field of createAgentReq — delete it (the field it guarded is gone)", name)
		}
	}
}

// TestCreateParamsMCPFieldsAreDeclaredBothWays checks the registry against the
// MCP input struct in both directions: a declared field must exist and be
// documented, and an MCP field nothing declares is an input that maps nowhere.
func TestCreateParamsMCPFieldsAreDeclaredBothWays(t *testing.T) {
	claimed := map[string]string{} // createAgentIn field -> createAgentReq field
	for name, p := range createParams {
		if p.MCP == "" {
			continue
		}
		f, ok := createInType.FieldByName(p.MCP)
		if !ok {
			t.Errorf("createParams[%q] says it is exposed as createAgentIn.%s, but createAgentIn has no such field — add it (with a jsonschema description) or change the entry to {MCPSkip: \"<why>\"}", name, p.MCP)
			continue
		}
		if strings.TrimSpace(f.Tag.Get("jsonschema")) == "" {
			t.Errorf("createAgentIn.%s has no jsonschema description — an MCP caller sees the parameter but not what it means, so it may as well be missing", p.MCP)
		}
		if prev, dup := claimed[p.MCP]; dup {
			t.Errorf("createAgentIn.%s is claimed by both createParams[%q] and createParams[%q]", p.MCP, prev, name)
		}
		claimed[p.MCP] = name
	}
	for i := 0; i < createInType.NumField(); i++ {
		f := createInType.Field(i)
		if _, ok := claimed[f.Name]; !ok {
			t.Errorf("createAgentIn.%s is advertised to MCP callers but no createParams entry claims it — it maps to no createAgentReq field, so anything a caller passes is discarded. Point a createParams entry at it, or drop the parameter.", f.Name)
		}
	}
}

// TestCreateParamsMCPValuesReachTheReq proves the mapping actually runs, not
// just that the names line up. Declaring a field in createAgentIn and then
// forgetting it in toCreateReq drops the value just as silently as never
// declaring it, so the probe drives every exposed field through the real
// mapping and checks it landed.
func TestCreateParamsMCPValuesReachTheReq(t *testing.T) {
	// Derived fields are transformed rather than copied, so they get a
	// hand-written assertion instead, keyed by createAgentReq field name. Every
	// MCPDerived field must have one — otherwise a transform could be silently
	// gutted and the copy check would never notice.
	derived := map[string]func(t *testing.T){
		"NoFocus": func(t *testing.T) {
			// The polarity flip is the whole point: default (focus unset) must
			// mean "don't steal the user's pane".
			if got := (createAgentIn{}).toCreateReq(); !got.NoFocus {
				t.Error("createAgentIn{} (focus unset) must map to NoFocus:true — an MCP spawn must not yank a watching user away from their pane")
			}
			if got := (createAgentIn{Focus: true}).toCreateReq(); got.NoFocus {
				t.Error("createAgentIn{Focus:true} must map to NoFocus:false — focus:true is the caller opting in to landing on the new pane")
			}
		},
		"Host": func(t *testing.T) {
			// createAgentTool resolves Host to a Backend and createAgent takes
			// the host from b.Name(); a copied req.Host would record an intent
			// nothing reads. Pin that so a future copy is a considered change.
			if got := (createAgentIn{Host: "gigachad"}).toCreateReq(); got.Host != "" {
				t.Errorf("toCreateReq set req.Host=%q, but createAgent ignores it and takes the host from the backend it is handed — resolve the host to a Backend in createAgentTool instead", got.Host)
			}
		},
	}

	probe := reflect.New(createInType).Elem()
	for i := 0; i < createInType.NumField(); i++ {
		f := createInType.Field(i)
		v, err := probeValue(f)
		if err != nil {
			t.Fatalf("createAgentIn.%s: %v", f.Name, err)
		}
		probe.Field(i).Set(v)
	}
	req := reflect.ValueOf(probe.Interface().(createAgentIn).toCreateReq())

	for name, p := range createParams {
		if p.MCP == "" {
			continue
		}
		if p.MCPDerived != "" {
			check, ok := derived[name]
			if !ok {
				t.Errorf("createParams[%q] is MCPDerived (%s) but has no case in this test's `derived` map — a transform nothing asserts is a transform that can be silently broken. Add one.", name, p.MCPDerived)
				continue
			}
			t.Run(name, check)
			continue
		}
		in, ok := createInType.FieldByName(p.MCP)
		if !ok {
			continue // reported by TestCreateParamsMCPFieldsAreDeclaredBothWays
		}
		want, err := probeValue(in)
		if err != nil {
			continue // already reported by the probe loop above
		}
		got := req.FieldByName(name)
		if !got.IsValid() {
			continue // reported by TestCreateParamsDeclareEveryField
		}
		if got.Type() != want.Type() {
			t.Errorf("createAgentIn.%s is %s but createAgentReq.%s is %s — the values cannot be copied across", p.MCP, want.Type(), name, got.Type())
			continue
		}
		if !reflect.DeepEqual(got.Interface(), want.Interface()) {
			t.Errorf(`createAgentIn.%s never reaches createAgentReq.%s: toCreateReq returned %#v, want %#v.

The parameter is advertised to MCP callers but its value is dropped on the way
in — the caller gets no error and no hint. Add `+"`%s: in.%s,`"+` to
createAgentIn.toCreateReq.`, p.MCP, name, got.Interface(), want.Interface(), name, p.MCP)
		}
	}
}

// probeValue returns a distinctive value for a createAgentIn field, so a copy
// that silently didn't happen shows up as a zero value.
func probeValue(f reflect.StructField) (reflect.Value, error) {
	switch f.Type.Kind() {
	case reflect.String:
		return reflect.ValueOf("probe-" + f.Name), nil
	case reflect.Bool:
		return reflect.ValueOf(true), nil
	case reflect.Int:
		return reflect.ValueOf(4242), nil
	case reflect.Slice:
		if f.Type.Elem().Kind() == reflect.String {
			return reflect.ValueOf([]string{"probe-" + f.Name}), nil
		}
	}
	return reflect.Value{}, fmt.Errorf("the parity test has no probe value for type %s — teach probeValue about it so the field is still checked", f.Type)
}

// ---------------------------------------------------------------------------
// the TypeScript leg
// ---------------------------------------------------------------------------

// tsAPIPath is relative to the Go module root (src/), where `go test` runs.
const tsAPIPath = "web/src/lib/api.ts"

// TestCreateParamsMatchTypeScriptPayload guards the third copy — the browser's
// CreateAgentPayload — by parsing the interface out of api.ts and comparing it
// against createAgentReq's json names.
//
// This is deliberately the cheap 80% rather than Go->TS codegen, which is more
// machinery than this codebase wants for one payload. It covers what actually
// drifts: a property present on one side and not the other, a misspelled wire
// name, and a grossly wrong type. It does NOT check that CreateAgentDialog
// actually *sets* the property (a field can be in the interface and never sent
// — TSSkip records intent, not behavior), nor optional/required parity, since
// Go zero values make every field optional on the wire anyway.
func TestCreateParamsMatchTypeScriptPayload(t *testing.T) {
	src, err := os.ReadFile(tsAPIPath)
	if err != nil {
		t.Fatalf("reading %s: %v (the TypeScript create payload is the third copy of this shape; this test compares against it)", tsAPIPath, err)
	}
	ts, err := tsInterfaceFields(string(src), "CreateAgentPayload")
	if err != nil {
		t.Fatalf("parsing CreateAgentPayload out of %s: %v", tsAPIPath, err)
	}

	claimed := map[string]bool{}
	for i := 0; i < createReqType.NumField(); i++ {
		f := createReqType.Field(i)
		p, ok := createParams[f.Name]
		if !ok || p.TS == "" {
			continue // undeclared / deliberately absent — reported elsewhere
		}
		claimed[p.TS] = true
		tsType, present := ts[p.TS]
		if !present {
			t.Errorf(`createAgentReq.%s is declared as CreateAgentPayload.%s, but %s has no such property.

Either add `+"`%s?: %s`"+` to the CreateAgentPayload interface (and send it from
CreateAgentDialog), or record {TSSkip: "<why the form doesn't send it>"} in
createParams.`, f.Name, p.TS, tsAPIPath, p.TS, tsTypeFor(f.Type))
			continue
		}
		if p.TS != jsonName(f) {
			t.Errorf("createAgentReq.%s serializes as %q but createParams maps it to CreateAgentPayload.%s — the browser's property would never bind", f.Name, jsonName(f), p.TS)
		}
		if !tsTypeMatches(f.Type, tsType) {
			t.Errorf("CreateAgentPayload.%s is %q but createAgentReq.%s is %s — expected %s", p.TS, tsType, f.Name, f.Type, tsTypeFor(f.Type))
		}
	}
	for name := range ts {
		if !claimed[name] {
			t.Errorf("CreateAgentPayload.%s (%s) matches no createAgentReq field the registry declares — the browser sends a property the server drops. Add the field to createAgentReq and declare it in createParams, or remove it from the interface.", name, tsAPIPath)
		}
	}
}

var tsFieldRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\??:\s*(.+?),?$`)

// tsInterfaceFields extracts an interface's property names and types from
// TypeScript source. The payload interfaces here are flat blocks of
// `name?: type` lines, so a line-wise read is enough — and an unparsable line
// is an error rather than a skip, so reshaping the interface into something
// this can't read fails loudly instead of silently guarding nothing.
func tsInterfaceFields(src, name string) (map[string]string, error) {
	head := "export interface " + name + " {"
	i := strings.Index(src, head)
	if i < 0 {
		return nil, fmt.Errorf("no %q found", head)
	}
	body := src[i+len(head):]
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end]
	} else {
		return nil, fmt.Errorf("interface %s is not closed by a `}` at column 0", name)
	}
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		m := tsFieldRe.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("cannot parse property line %q — this parser handles flat `name?: type` interfaces only", line)
		}
		out[m[1]] = strings.TrimSpace(m[2])
	}
	return out, nil
}

// tsTypeFor is the TypeScript type a Go field is expected to carry, used in
// failure messages so the fix is copy-pasteable.
func tsTypeFor(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int:
		return "number"
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String {
			return "string[]"
		}
	}
	return t.String()
}

// tsTypeMatches is a loose Go/TS type comparison: enough to catch a string
// where an array belongs, permissive enough for the string-literal unions the
// payload uses (`"git" | "scratch"`).
func tsTypeMatches(goType reflect.Type, ts string) bool {
	ts = strings.TrimSpace(ts)
	if ts == tsTypeFor(goType) {
		return true
	}
	if goType.Kind() == reflect.String {
		// A union of quoted literals is a narrowed string.
		for _, part := range strings.Split(ts, "|") {
			part = strings.TrimSpace(part)
			if len(part) < 2 || part[0] != '"' || part[len(part)-1] != '"' {
				return false
			}
		}
		return true
	}
	return false
}
