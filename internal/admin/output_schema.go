package admin

import (
	"fmt"
	"reflect"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/metrics"
	"github.com/google/jsonschema-go/jsonschema"
)

func outputSchemaFor[T any]() *jsonschema.Schema {
	opts := &jsonschema.ForOptions{TypeSchemas: make(map[reflect.Type]*jsonschema.Schema)}
	// jsonschema-go infers nullable slices and pointers, but not maps. These
	// response maps can marshal to null; retain their inferred value contracts.
	for _, typ := range []reflect.Type{
		reflect.TypeFor[map[string]int64](),
		reflect.TypeFor[map[string]*metrics.ServerMetricsSnapshot](),
	} {
		schema, err := jsonschema.ForType(typ, opts)
		if err != nil {
			panic(fmt.Sprintf("admin output schema: %v", err))
		}
		schema.Type = ""
		schema.Types = []string{"object", "null"}
		opts.TypeSchemas[typ] = schema
	}
	schema, err := jsonschema.For[T](opts)
	if err != nil {
		panic(fmt.Sprintf("admin output schema: %v", err))
	}
	return schema
}
