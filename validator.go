package jsonrpc

import (
	"bytes"

	"github.com/santhosh-tekuri/jsonschema/v2"
)

// Validator defines a contract of JSON Schema validator.
type Validator interface {
	ValidateParams(method string, jsonBody []byte) error
	ValidateResult(method string, jsonBody []byte) error

	AddParamsSchema(method string, jsonSchema []byte) error
	AddResultSchema(method string, jsonSchema []byte) error
}

// JSONSchemaValidator implements Validator with JSON Schema.
type JSONSchemaValidator struct {
	paramsSchema map[string]*jsonschema.Schema
	resultSchema map[string]*jsonschema.Schema
}

func (jv *JSONSchemaValidator) addSchema(method string, isParams bool, jsonSchema []byte) error {
	compiler := jsonschema.NewCompiler()

	err := compiler.AddResource("schema.json", bytes.NewBuffer(jsonSchema))
	if err != nil {
		return err
	}

	schema, err := compiler.Compile("schema.json")
	if err != nil {
		return err
	}

	if isParams {
		if jv.paramsSchema == nil {
			jv.paramsSchema = make(map[string]*jsonschema.Schema)
		}

		jv.paramsSchema[method] = schema
	} else {
		if jv.resultSchema == nil {
			jv.resultSchema = make(map[string]*jsonschema.Schema)
		}

		jv.resultSchema[method] = schema
	}

	return nil
}

// AddParamsSchema registers parameters schema.
func (jv *JSONSchemaValidator) AddParamsSchema(method string, jsonSchema []byte) error {
	return jv.addSchema(method, true, jsonSchema)
}

// AddResultSchema registers result schema.
func (jv *JSONSchemaValidator) AddResultSchema(method string, jsonSchema []byte) error {
	return jv.addSchema(method, false, jsonSchema)
}

// ValidateParams validates parameters value with JSON schema.
func (jv *JSONSchemaValidator) ValidateParams(method string, jsonBody []byte) error {
	return jv.validate(method, true, jsonBody)
}

// ValidateResult validates result value with JSON schema.
func (jv *JSONSchemaValidator) ValidateResult(method string, jsonBody []byte) error {
	return jv.validate(method, false, jsonBody)
}

// ValidateJSONBody performs validation of JSON body.
func (jv *JSONSchemaValidator) validate(method string, isParams bool, jsonBody []byte) error {
	store := jv.paramsSchema
	name := "params"

	if !isParams {
		store = jv.resultSchema
		name = "result"
	}

	schema, found := store[method]
	if !found {
		return nil
	}

	err := schema.Validate(bytes.NewReader(jsonBody))
	if err == nil {
		return nil
	}

	errs := make(ValidationErrors, 1)

	//nolint:errorlint // Error is not wrapped, type assertion is more performant.
	if ve, ok := err.(*jsonschema.ValidationError); ok {
		errs[name] = appendError(errs[name], ve)
	} else {
		errs[name] = append(errs[name], err.Error())
	}

	return errs
}

func appendError(errorMessages []string, err *jsonschema.ValidationError) []string {
	errorMessages = append(errorMessages, err.InstancePtr+": "+err.Message)
	for _, ec := range err.Causes {
		errorMessages = appendError(errorMessages, ec)
	}

	return errorMessages
}
