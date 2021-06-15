package jsonrpc

import (
	"encoding/json"
	"fmt"
	"net/http"
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

func (c *OpenAPI) setupOutput(oc *openapi3.OperationContext, u usecase.Interactor) error {
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
