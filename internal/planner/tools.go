package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/giammbo/prosa/internal/engine"
	"github.com/giammbo/prosa/internal/lock"
)

// The planner's tools, in the fixed order they are sent.
const (
	toolSearch  = "search_resource_types"
	toolSchema  = "get_resource_schema"
	toolSubmit  = "submit_desired_state"
	toolChanges = "submit_changes"
)

// searchLimit is how many resource types a search returns.
const searchLimit = 20

// tools returns the tool definitions, always the same bytes (prompt cache),
// for any backend. No tool is strict: a resource's config is a free-form
// object, which strict schemas cannot express, so parseSubmission checks the
// shape itself.
func tools() []ToolDef {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	search := ToolDef{
		Name:        toolSearch,
		Description: "Search the resource types of the AWS provider by keywords (for example \"rds instance\", \"ecs service\", \"security group\"). Call it to find the type names to use before reading their schemas. Returns the best matching type names, one per line.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": str("Keywords, separated by spaces.")},
			"required":   []string{"query"},
		},
	}
	schema := ToolDef{
		Name:        toolSchema,
		Description: "Return the schema of an AWS provider resource type: its attributes with type, flags (required, optional, computed, sensitive, write_only) and description, then its nested blocks. Call it for every type you use before submitting it. To read a nested block that the schema shows only as a summary, pass \"<type>.<block>[.<block>...]\".",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"type": str("A resource type name such as aws_db_instance, or \"<type>.<block>\" for a nested block.")},
			"required":   []string{"type"},
		},
	}
	resource := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key":  str("Stable short key of the resource within the intent, such as \"db.instance\"."),
			"type": str("Resource type, such as aws_db_instance."),
			"config": map[string]any{"type": "object",
				"description": "The resource configuration: a JSON object with the attributes and nested blocks of the schema."},
			"depends_on": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "Resources, as \"<intent>/<key>\", that must exist before this one although the config does not refer to them (an ECS service after its HTTPS listener). Leave it out when the references already give the order."},
		},
		"required":             []string{"key", "type", "config"},
		"additionalProperties": false,
	}
	enum := func(desc string, values ...string) map[string]any {
		return map[string]any{"type": "string", "enum": values, "description": desc}
	}
	integer := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	check := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":     str("The sentence of the intent's \"Checks\" section (or \"Verifiche\", its older name) that the check verifies, exactly as written."),
			"type":     enum("The kind of check; each uses only its own fields.", "resource_attr", "http_get", "tls_days", "unverifiable"),
			"resource": str("resource_attr: the resource, \"<intent>/<key>\", of this intent or of an intent in its depends_on."),
			"path":     str("resource_attr: the attribute path in the resource's schema, dotted, list elements by index: \"publicly_accessible\", \"master_user_secret.0.secret_arn\"."),
			"op":       enum("resource_attr: how the attribute is compared with value.", "eq", "ne", "gte", "lte", "contains", "matches", "within", "exists"),
			"value":    map[string]any{"description": "resource_attr: the value to compare with, of the attribute's type; a regular expression for matches, a duration such as \"24h\" for within (the attribute is a timestamp at most that old); left out for exists."},
			"url":      str("http_get: the https URL to GET."),
			"status":   integer("http_get: the expected HTTP status; 200 when left out."),
			"within":   str("http_get: how long to keep trying, a duration such as \"60s\"; 60s when left out."),
			"host":     str("tls_days: the host whose certificate is checked on port 443."),
			"min_days": integer("tls_days: the minimum number of days the certificate must still be valid."),
			"reason":   str("unverifiable: why no check type can verify the sentence."),
		},
		"required":             []string{"name", "type"},
		"additionalProperties": false,
	}
	submit := ToolDef{
		Name:        toolSubmit,
		Description: "Submit the complete desired state of the intent: every resource it needs, not only the changed ones, and one check per sentence of its \"Checks\" section (or \"Verifiche\", its older name). Always finish by calling it. If the state has problems, they come back as an error to fix before submitting again. If the intent contradicts itself or asks for something impossible, submit blockers with the reasons and the previous resources unchanged.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resources": map[string]any{"type": "array", "items": resource, "description": "The complete set of resources of the intent."},
				"checks": map[string]any{"type": "array", "items": check,
					"description": "The checks of the intent, one per sentence of its \"Checks\" section (or \"Verifiche\", its older name), in their order; empty or left out when it has none."},
				"blockers": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Reasons the intent cannot be planned as written; empty or left out when it can."},
			},
			"required":             []string{"resources"},
			"additionalProperties": false,
		},
	}
	op := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"op":         enum("The operation: set a value, unset one, add a resource, remove one, replace its depends_on, or replace the checks.", "set", "unset", "add", "remove", "depends_on", "checks"),
			"key":        str("set, unset, remove, depends_on: the key of the resource to change; add: the key of the new resource."),
			"path":       str("set, unset: where in the resource's config, dotted, list elements by index (\"ingress.0.from_port\"). set writes the value there, creating missing objects; a block that is a list or a set is replaced whole."),
			"value":      map[string]any{"description": "set: the new value. depends_on: the complete list of nodes, \"<intent>/<key>\". checks: the complete list of checks, in the format of submit_desired_state."},
			"type":       str("add: the resource type."),
			"config":     map[string]any{"type": "object", "description": "add: the resource configuration."},
			"depends_on": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "add: the nodes the new resource comes after without referring to them."},
		},
		"required":             []string{"op"},
		"additionalProperties": false,
	}
	changes := ToolDef{
		Name:        toolChanges,
		Description: "Submit only what changes in the state you were shown (the base, or the previous resources): a list of operations, applied in order, on the config as it appears there. An empty list means nothing changes. Use it instead of submit_desired_state whenever the request gives a base or previous resources. If an operation does not apply (a key or path that does not exist) or the result has problems, they come back as an error to fix before submitting again. If the intent contradicts itself or asks for something impossible, submit blockers.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"changes": map[string]any{"type": "array", "items": op,
					"description": "The operations, in order; empty when nothing changes."},
				"blockers": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"description": "Reasons the intent cannot be planned as written; empty or left out when it can."},
			},
			"required":             []string{"changes"},
			"additionalProperties": false,
		},
	}
	return []ToolDef{search, schema, submit, changes}
}

// runSearch answers search_resource_types; the bool is is_error.
func runSearch(cat engine.SchemaCatalog, input string) (string, bool) {
	var in struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(input), &in); err != nil || strings.TrimSpace(in.Query) == "" {
		return "invalid input: expected {\"query\": \"<keywords>\"}", true
	}
	hits := cat.Search(in.Query, searchLimit)
	if len(hits) == 0 {
		return fmt.Sprintf("no resource type matches %q", in.Query), false
	}
	return strings.Join(hits, "\n"), false
}

// runSchema answers get_resource_schema; the bool is is_error.
func runSchema(cat engine.SchemaCatalog, input string) (string, bool) {
	var in struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(input), &in); err != nil || in.Type == "" {
		return "invalid input: expected {\"type\": \"<resource type>\"}", true
	}
	text, err := cat.Describe(in.Type)
	if err != nil {
		return err.Error(), true
	}
	return text, false
}

// parseSubmission decodes a submit_desired_state input, numbers kept as
// json.Number, and checks its shape: no unknown fields, "resources"
// present, each with a non-empty key and type, a JSON object as config and
// an optional depends_on list, and optional checks (depends_on and checks
// are checked by the engine's validation).
// Blank blockers are dropped. A malformed input is one problem; otherwise
// there is one problem per bad resource, prefixed with "<intent>/<key>"
// like the engine's.
func parseSubmission(intentName, input string) (engine.Desired, []string) {
	var in struct {
		Resources *[]struct {
			Key       string          `json:"key"`
			Type      string          `json:"type"`
			Config    json.RawMessage `json:"config"`
			DependsOn []string        `json:"depends_on"`
		} `json:"resources"`
		Checks   []lock.Check `json:"checks"`
		Blockers []string     `json:"blockers"`
	}
	dec := json.NewDecoder(strings.NewReader(input))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return engine.Desired{}, []string{fmt.Sprintf("invalid submit_desired_state input: %s", jsonProblem(err))}
	}
	if in.Resources == nil {
		return engine.Desired{}, []string{`invalid submit_desired_state input: "resources" is required (an empty list when the intent needs nothing)`}
	}
	d := engine.Desired{Checks: in.Checks}
	for _, b := range in.Blockers {
		if b = strings.TrimSpace(b); b != "" {
			d.Blockers = append(d.Blockers, b)
		}
	}
	var probs []string
	for _, r := range *in.Resources {
		bad := func(msg string) { probs = append(probs, fmt.Sprintf("%s/%s: %s", intentName, r.Key, msg)) }
		switch {
		case r.Key == "":
			bad("resource with an empty key")
			continue
		case r.Type == "":
			bad("resource has no type")
			continue
		}
		cfg, err := decodeConfig(r.Config)
		if err != nil {
			bad(err.Error())
			continue
		}
		d.Resources = append(d.Resources, engine.DesiredResource{Key: r.Key, Type: r.Type, Config: cfg, DependsOn: r.DependsOn})
	}
	return d, probs
}

// parseChanges decodes a submit_changes input, numbers kept as json.Number:
// no unknown fields (in the operations too), "changes" present, and blank
// blockers dropped. The Desired it returns is only a Patch (never nil, empty
// for no changes), or only Blockers. A malformed input is one problem; the
// operations themselves are checked when the patch is applied.
func parseChanges(input string) (engine.Desired, []string) {
	var in struct {
		Changes  *[]lock.PatchOp `json:"changes"`
		Blockers []string        `json:"blockers"`
	}
	dec := json.NewDecoder(strings.NewReader(input))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return engine.Desired{}, []string{fmt.Sprintf("invalid submit_changes input: %s", jsonProblem(err))}
	}
	var blockers []string
	for _, b := range in.Blockers {
		if b = strings.TrimSpace(b); b != "" {
			blockers = append(blockers, b)
		}
	}
	if len(blockers) > 0 {
		return engine.Desired{Blockers: blockers}, nil
	}
	if in.Changes == nil {
		return engine.Desired{}, []string{`invalid submit_changes input: "changes" is required (an empty list when nothing changes)`}
	}
	patch := *in.Changes
	if patch == nil {
		patch = []lock.PatchOp{}
	}
	return engine.Desired{Patch: patch}, nil
}

// decodeConfig decodes raw, which must be a JSON object, keeping numbers as
// json.Number.
func decodeConfig(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, errors.New("config must be a JSON object, but it is missing")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var cfg map[string]any
	if err := dec.Decode(&cfg); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return nil, fmt.Errorf("config must be a JSON object, got %s", jsonKindName(te.Value))
		}
		return nil, fmt.Errorf("config must be a JSON object: %s", jsonProblem(err))
	}
	if cfg == nil {
		return nil, errors.New("config must be a JSON object, got null")
	}
	return cfg, nil
}

// jsonKindName names a JSON kind as encoding/json reports it ("array",
// "string", ...) the way the planner's other messages do.
func jsonKindName(kind string) string {
	if kind == "array" {
		return "a list"
	}
	if strings.HasPrefix(kind, "number ") {
		return "a number"
	}
	if kind == "object" {
		return "an object"
	}
	return "a " + kind
}

// goKindName describes a Go type as the JSON value it decodes from, without
// naming the Go type.
func goKindName(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		return "a list"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a bool"
	case reflect.Struct, reflect.Map:
		return "an object"
	}
	return "a number"
}

// jsonProblem rewrites an encoding/json error without Go types, for the
// planner: "resources must be a list, got a string".
func jsonProblem(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		field := te.Field
		if field == "" {
			field = "the input"
		}
		return fmt.Sprintf("%s must be %s, got %s", field, goKindName(te.Type), jsonKindName(te.Value))
	}
	return strings.TrimPrefix(err.Error(), "json: ")
}

// problemLines renders a validation error as one problem per line,
// prefixed with "<intent>/<key>".
func problemLines(ve *engine.ValidationError) []string {
	if len(ve.Problems) == 0 {
		return []string{ve.Error()}
	}
	out := make([]string, len(ve.Problems))
	for i, p := range ve.Problems {
		out[i] = fmt.Sprintf("%s/%s: %s", p.Intent, p.Key, p.Message)
	}
	return out
}
