package jsonrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/swaggest/openapi-go/openapi3"
	"github.com/swaggest/rest"
	"github.com/swaggest/usecase"
)

// OpenAPI extracts OpenAPI documentation from HTTP handler and underlying use case interactor.
type OpenAPI struct {
	mu sync.Mutex

	BasePath    string // URL path to docs, default "/docs/".
	gen         *openapi3.Reflector
	annotations map[string][]func(*openapi3.Operation) error
}

// Reflector is an accessor to OpenAPI Reflector instance.
func (c *OpenAPI) Reflector() *openapi3.Reflector {
	if c.gen == nil {
		c.gen = &openapi3.Reflector{}
	}

	return c.gen
}

// Annotate adds OpenAPI operation configuration that is applied during collection.
func (c *OpenAPI) Annotate(name string, setup ...func(op *openapi3.Operation) error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.annotations == nil {
		c.annotations = make(map[string][]func(op *openapi3.Operation) error)
	}

	c.annotations[name] = append(c.annotations[name], setup...)
}

// Collect adds use case handler to documentation.
func (c *OpenAPI) Collect(
	name string,
	u usecase.Interactor,
	annotations ...func(*openapi3.Operation) error,
) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	defer func() {
		if err != nil {
			err = fmt.Errorf("failed to reflect API schema for %s: %w", name, err)
		}
	}()

	reflector := c.Reflector()

	err = reflector.SpecEns().SetupOperation(http.MethodPost, name, func(op *openapi3.Operation) error {
		oc := openapi3.OperationContext{
			Operation:       op,
			HTTPMethod:      http.MethodPost,
			HTTPStatus:      http.StatusOK,
			RespContentType: "application/json",
		}

		err = c.setupInput(&oc, u)
		if err != nil {
			return fmt.Errorf("failed to setup request: %w", err)
		}

		err = c.setupOutput(&oc, u)
		if err != nil {
			return fmt.Errorf("failed to setup response: %w", err)
		}

		err = c.processUseCase(op, u)
		if err != nil {
			return err
		}

		for _, setup := range c.annotations[name] {
			err = setup(op)
			if err != nil {
				return err
			}
		}

		for _, setup := range annotations {
			err = setup(op)
			if err != nil {
				return err
			}
		}

		return nil
	})

	return err
}

func (c *OpenAPI) setupOutput(oc *openapi3.OperationContext, u usecase.Interactor) error {
	var (
		hasOutput usecase.HasOutputPort
		status    = http.StatusOK
	)

	if usecase.As(u, &hasOutput) {
		oc.Output = hasOutput.OutputPort()

		if rest.OutputHasNoContent(oc.Output) {
			status = http.StatusNoContent
		}
	} else {
		status = http.StatusNoContent
	}

	if oc.HTTPStatus == 0 {
		oc.HTTPStatus = status
	}

	err := c.Reflector().SetupResponse(*oc)
	if err != nil {
		return err
	}

	if oc.HTTPMethod == http.MethodHead {
		for code, resp := range oc.Operation.Responses.MapOfResponseOrRefValues {
			for contentType, cont := range resp.Response.Content {
				cont.Schema = nil
				resp.Response.Content[contentType] = cont
			}

			oc.Operation.Responses.MapOfResponseOrRefValues[code] = resp
		}
	}

	return nil
}

func (c *OpenAPI) setupInput(oc *openapi3.OperationContext, u usecase.Interactor) error {
	var (
		hasInput usecase.HasInputPort

		err error
	)

	if usecase.As(u, &hasInput) {
		oc.Input = hasInput.InputPort()

		err = c.Reflector().SetupRequest(*oc)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *OpenAPI) processUseCase(op *openapi3.Operation, u usecase.Interactor) error {
	var (
		hasName        usecase.HasName
		hasTitle       usecase.HasTitle
		hasDescription usecase.HasDescription
		hasTags        usecase.HasTags
		hasDeprecated  usecase.HasIsDeprecated
	)

	if usecase.As(u, &hasName) {
		op.WithID(hasName.Name())
	}

	if usecase.As(u, &hasTitle) {
		op.WithSummary(hasTitle.Title())
	}

	if usecase.As(u, &hasTags) {
		op.WithTags(hasTags.Tags()...)
	}

	if usecase.As(u, &hasDescription) {
		op.WithDescription(hasDescription.Description())
	}

	if usecase.As(u, &hasDeprecated) && hasDeprecated.IsDeprecated() {
		op.WithDeprecated(true)
	}

	return nil
}

func (c *OpenAPI) provideParametersJSONSchemas(op openapi3.Operation, validator rest.JSONSchemaValidator) error {
	for _, p := range op.Parameters {
		if p.Parameter.Schema == nil {
			continue
		}

		pp := p.Parameter
		schema := pp.Schema.ToJSONSchema(c.Reflector().Spec)

		var (
			err        error
			schemaData []byte
		)

		if !schema.IsTrivial(c.Reflector().ResolveJSONSchemaRef) {
			schemaData, err = schema.JSONSchemaBytes()
			if err != nil {
				return fmt.Errorf("failed to build JSON Schema for parameter (%s, %s)", pp.In, pp.Name)
			}
		}

		required := false
		if pp.Required != nil && *pp.Required {
			required = true
		}

		if validator != nil {
			err = validator.AddSchema(rest.ParamIn(pp.In), pp.Name, schemaData, required)
			if err != nil {
				return fmt.Errorf("failed to add validation schema for parameter (%s, %s): %w", pp.In, pp.Name, err)
			}
		}
	}

	return nil
}

// ProvideRequestJSONSchemas provides JSON Schemas for request structure.
func (c *OpenAPI) ProvideRequestJSONSchemas(
	method string,
	input interface{},
	mapping rest.RequestMapping,
	validator rest.JSONSchemaValidator,
) error {
	op := openapi3.Operation{}
	oc := openapi3.OperationContext{
		Operation:  &op,
		HTTPMethod: method,
		Input:      input,
	}

	err := c.Reflector().SetupRequest(oc)
	if err != nil {
		return err
	}

	err = c.provideParametersJSONSchemas(op, validator)
	if err != nil {
		return err
	}

	if op.RequestBody != nil && op.RequestBody.RequestBody != nil {
		for ct, content := range op.RequestBody.RequestBody.Content {
			var in rest.ParamIn

			switch ct {
			case "application/json":
				in = rest.ParamInBody
			default:
				continue
			}

			schema := content.Schema.ToJSONSchema(c.Reflector().Spec)
			if schema.IsTrivial(c.Reflector().ResolveJSONSchemaRef) {
				continue
			}

			schemaData, err := schema.JSONSchemaBytes()
			if err != nil {
				return errors.New("failed to build JSON Schema for request body")
			}

			err = validator.AddSchema(in, "body", schemaData, false)
			if err != nil {
				return fmt.Errorf("failed to add validation schema for request body: %w", err)
			}
		}
	}

	return nil
}

func (c *OpenAPI) provideHeaderSchemas(resp *openapi3.Response, validator rest.JSONSchemaValidator) error {
	for name, h := range resp.Headers {
		if h.Header.Schema == nil {
			continue
		}

		hh := h.Header
		schema := hh.Schema.ToJSONSchema(c.Reflector().Spec)

		var (
			err        error
			schemaData []byte
		)

		if !schema.IsTrivial(c.Reflector().ResolveJSONSchemaRef) {
			schemaData, err = schema.JSONSchemaBytes()
			if err != nil {
				return fmt.Errorf("failed to build JSON Schema for response header (%s)", name)
			}
		}

		required := false
		if hh.Required != nil && *hh.Required {
			required = true
		}

		if validator != nil {
			err = validator.AddSchema(rest.ParamInHeader, name, schemaData, required)
			if err != nil {
				return fmt.Errorf("failed to add validation schema for response header (%s): %w", name, err)
			}
		}
	}

	return nil
}

// ProvideResponseJSONSchemas provides JSON schemas for response structure.
func (c *OpenAPI) ProvideResponseJSONSchemas(
	statusCode int,
	contentType string,
	output interface{},
	headerMapping map[string]string,
	validator rest.JSONSchemaValidator,
) error {
	op := openapi3.Operation{}
	oc := openapi3.OperationContext{
		Operation:         &op,
		HTTPStatus:        statusCode,
		Output:            output,
		RespHeaderMapping: headerMapping,
		RespContentType:   contentType,
	}

	if err := c.Reflector().SetupResponse(oc); err != nil {
		return err
	}

	resp := op.Responses.MapOfResponseOrRefValues[strconv.Itoa(statusCode)].Response

	if err := c.provideHeaderSchemas(resp, validator); err != nil {
		return err
	}

	for _, cont := range resp.Content {
		if cont.Schema == nil {
			continue
		}

		schema := cont.Schema.ToJSONSchema(c.Reflector().Spec)

		if schema.IsTrivial(c.Reflector().ResolveJSONSchemaRef) {
			continue
		}

		schemaData, err := schema.JSONSchemaBytes()
		if err != nil {
			return errors.New("failed to build JSON Schema for response body")
		}

		if err := validator.AddSchema(rest.ParamInBody, "body", schemaData, false); err != nil {
			return fmt.Errorf("failed to add validation schema for response body: %w", err)
		}
	}

	return nil
}

func (c *OpenAPI) ServeHTTP(rw http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()

	document, err := json.MarshalIndent(c.Reflector().Spec, "", " ")
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
	}

	rw.Header().Set("Content-Type", "application/json; charset=utf8")

	_, err = rw.Write(document)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
	}
}
