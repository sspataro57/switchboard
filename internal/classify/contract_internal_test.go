package classify

// SWT-33 re-review: Contract.DecisionKey and CategoryKey are read by the eval's
// label token and named by the runbook, while the DECODE goes through the
// verdict structs' JSON tags. Two spellings of one fact — this pins them
// together, in-package because the verdict structs are unexported.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestContracts_KeysAreTheVerdictStructsTagsAndTheSchemasProperties(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contract Contract
		verdict  any
	}{
		{"actionability", ActionabilityContract, verdict{}},
		{"inquiry", InquiryContract, inquiryVerdict{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kinds := map[string]reflect.Kind{}
			rt := reflect.TypeOf(tc.verdict)
			for i := 0; i < rt.NumField(); i++ {
				f := rt.Field(i)
				kinds[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Type.Kind()
			}
			if kinds[tc.contract.DecisionKey] != reflect.Bool {
				t.Errorf("Contract.DecisionKey %q is not a bool JSON field of the verdict struct (%v); the eval "+
					"and the fields assembly would read different keys", tc.contract.DecisionKey, kinds)
			}
			if kinds[tc.contract.CategoryKey] != reflect.String {
				t.Errorf("Contract.CategoryKey %q is not a string JSON field of the verdict struct (%v)",
					tc.contract.CategoryKey, kinds)
			}

			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(tc.contract.Schema, &schema); err != nil {
				t.Fatalf("contract schema does not parse: %v", err)
			}
			// The struct mirrors the schema, field for field: a property the struct
			// does not decode is an answer the model gives and nobody reads.
			for p := range schema.Properties {
				if _, ok := kinds[p]; !ok {
					t.Errorf("schema property %q has no field in the verdict struct", p)
				}
			}
			for k := range kinds {
				if _, ok := schema.Properties[k]; !ok {
					t.Errorf("verdict struct field %q is not a schema property", k)
				}
			}
		})
	}
}
