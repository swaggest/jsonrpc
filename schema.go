package jsonrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/swaggest/openapi-go/openapi3"
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
	v Validator,
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

	reflector.SpecEns().WithMapOfAnythingItem("x-envelope", "jsonrpc-2.0")

	err = reflector.SpecEns().SetupOperation(http.MethodPost, name, func(op *openapi3.Operation) error {
		oc := openapi3.OperationContext{
			Operation:       op,
			HTTPMethod:      http.MethodPost,
			HTTPStatus:      http.StatusOK,
			RespContentType: "application/json",
		}

		err = c.setupInput(&oc, u, name, v)
		if err != nil {
			return fmt.Errorf("failed to setup request: %w", err)
		}

		err = c.setupOutput(&oc, u, name, v)
		if err != nil {
			return fmt.Errorf("failed to setup response: %w", err)
		}

		c.processUseCase(op, u)

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

func (c *OpenAPI) setupOutput(oc *openapi3.OperationContext, u usecase.Interactor, method string, v Validator) error {
	var (
		hasOutput usecase.HasOutputPort
		status    = http.StatusOK
	)

	if usecase.As(u, &hasOutput) {
		oc.Output = hasOutput.OutputPort()
	}

	if oc.HTTPStatus == 0 {
		oc.HTTPStatus = status
	}

	err := c.Reflector().SetupResponse(*oc)
	if err != nil {
		return err
	}

	if v != nil {
		return c.provideResponseJSONSchemas(method, oc.Operation, v)
	}

	return nil
}

func (c *OpenAPI) setupInput(oc *openapi3.OperationContext, u usecase.Interactor, method string, v Validator) error {
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

		if v != nil {
			return c.provideRequestJSONSchema(method, oc.Operation, v)
		}
	}

	return nil
}

func (c *OpenAPI) processUseCase(op *openapi3.Operation, u usecase.Interactor) {
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

// ProvideRequestJSONSchemas provides JSON Schemas for request structure.
func (c *OpenAPI) provideRequestJSONSchema(
	method string,
	op *openapi3.Operation,
	validator Validator,
) error {
	if op.RequestBody != nil && op.RequestBody.RequestBody != nil {
		for ct, content := range op.RequestBody.RequestBody.Content {
			if ct != "application/json" {
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

			err = validator.AddParamsSchema(method, schemaData)
			if err != nil {
				return fmt.Errorf("failed to add validation schema for request body: %w", err)
			}
		}
	}

	return nil
}

// provideResponseJSONSchemas provides JSON schemas for response structure.
func (c *OpenAPI) provideResponseJSONSchemas(
	method string,
	op *openapi3.Operation,
	validator Validator,
) error {
	resp := op.Responses.MapOfResponseOrRefValues[strconv.Itoa(http.StatusOK)].Response

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

		if err := validator.AddResultSchema(method, schemaData); err != nil {
			return fmt.Errorf("failed to add validation schema for response body: %w", err)
		}
	}

	return nil
}
