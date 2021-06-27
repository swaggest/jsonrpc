package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"reflect"
	"sync"

	"github.com/swaggest/usecase"
)

// ErrorCode is an JSON-RPC 2.0 error code.
type ErrorCode int

// Standard error codes.
const (
	CodeParseError     = ErrorCode(-32700)
	CodeInvalidRequest = ErrorCode(-32600)
	CodeMethodNotFound = ErrorCode(-32601)
	CodeInvalidParams  = ErrorCode(-32602)
	CodeInternalError  = ErrorCode(-32603)
)

const ver = "2.0"

// Handler serves JSON-RPC 2.0 methods with HTTP.
type Handler struct {
	OpenAPI     *OpenAPI
	Validator   Validator
	Middlewares []usecase.Middleware

	SkipParamsValidation bool
	SkipResultValidation bool

	methods map[string]method
}

type method struct {
	// failingUseCase allows to pass input decoding error through use case middlewares.
	failingUseCase usecase.Interactor

	useCase usecase.Interactor

	inputBufferType  reflect.Type
	outputBufferType reflect.Type
}

func (h *method) setupInputBuffer() {
	h.inputBufferType = nil

	var withInput usecase.HasInputPort
	if !usecase.As(h.useCase, &withInput) {
		return
	}

	h.inputBufferType = reflect.TypeOf(withInput.InputPort())
	if h.inputBufferType != nil {
		if h.inputBufferType.Kind() == reflect.Ptr {
			h.inputBufferType = h.inputBufferType.Elem()
		}
	}
}

func (h *method) setupOutputBuffer() {
	h.outputBufferType = nil

	var withOutput usecase.HasOutputPort
	if !usecase.As(h.useCase, &withOutput) {
		return
	}

	h.outputBufferType = reflect.TypeOf(withOutput.OutputPort())
	if h.outputBufferType != nil {
		if h.outputBufferType.Kind() == reflect.Ptr {
			h.outputBufferType = h.outputBufferType.Elem()
		}
	}
}

type errCtxKey struct{}

// Add registers use case interactor as JSON-RPC method.
func (h *Handler) Add(u usecase.Interactor) {
	if h.methods == nil {
		h.methods = make(map[string]method)
	}

	var withName usecase.HasName

	if !usecase.As(u, &withName) {
		panic("use case name is required")
	}

	var fu usecase.Interactor = usecase.Interact(func(ctx context.Context, input, output interface{}) error {
		return ctx.Value(errCtxKey{}).(error)
	})

	u = usecase.Wrap(u, h.Middlewares...)
	fu = usecase.Wrap(fu, h.Middlewares...)

	m := method{
		useCase:        u,
		failingUseCase: fu,
	}
	m.setupInputBuffer()
	m.setupOutputBuffer()

	h.methods[withName.Name()] = m

	if h.OpenAPI != nil {
		err := h.OpenAPI.Collect(withName.Name(), u, h.Validator)
		if err != nil {
			panic(fmt.Sprintf("failed to add to OpenAPI schema: %s", err.Error()))
		}
	}
}

// Request is an JSON-RPC request item.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      *interface{}    `json:"id,omitempty"`
}

// Response is an JSON-RPC response item.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	ID      *interface{}    `json:"id"`
}

// Error describes JSON-RPC error structure.
type Error struct {
	Code    ErrorCode   `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

var errEmptyBody = errors.New("empty body")

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset: utf-8")

	reqBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		h.fail(w, fmt.Errorf("failed to read request body: %w", err), CodeParseError)

		return
	}

	reqBody = bytes.TrimLeft(reqBody, " \t\r\n")
	if len(reqBody) == 0 {
		h.fail(w, errEmptyBody, CodeParseError)

		return
	}

	ctx := r.Context()

	if reqBody[0] == '[' {
		h.serveBatch(ctx, w, reqBody)

		return
	}

	var (
		req  Request
		resp Response
	)

	if err := json.Unmarshal(reqBody, &req); err != nil {
		h.fail(w, fmt.Errorf("failed to unmarshal request: %w", err), CodeParseError)

		return
	}

	resp.ID = req.ID
	resp.JSONRPC = ver

	if req.JSONRPC != ver {
		h.fail(w, fmt.Errorf("invalid jsonrpc value: %q", req.JSONRPC), CodeInvalidRequest)

		return
	}

	h.invoke(ctx, req, &resp)

	if req.ID == nil {
		return
	}

	data, err := json.Marshal(resp)
	if err != nil {
		h.fail(w, err, CodeInternalError)

		return
	}

	if _, err := w.Write(data); err != nil {
		h.fail(w, err, CodeInternalError)
	}
}

func (h *Handler) serveBatch(ctx context.Context, w http.ResponseWriter, reqBody []byte) {
	var reqs []Request
	if err := json.Unmarshal(reqBody, &reqs); err != nil {
		h.fail(w, fmt.Errorf("failed to unmarshal request: %w", err), CodeInvalidRequest)

		return
	}

	wg := sync.WaitGroup{}
	wg.Add(len(reqs))

	resps := make([]*Response, 0, len(reqs))

	for _, req := range reqs {
		req := req
		resp := Response{
			JSONRPC: ver,
		}

		if req.ID != nil {
			resp.ID = req.ID
			resps = append(resps, &resp)
		}

		if req.JSONRPC != ver {
			resp.Error = &Error{
				Code:    CodeInvalidRequest,
				Message: fmt.Sprintf("invalid jsonrpc value: %q", req.JSONRPC),
			}

			continue
		}

		go func() {
			defer wg.Done()

			h.invoke(ctx, req, &resp)
		}()
	}

	wg.Wait()

	data, err := json.Marshal(resps)
	if err != nil {
		h.fail(w, err, CodeInternalError)

		return
	}

	if _, err := w.Write(data); err != nil {
		h.fail(w, err, CodeInternalError)
	}
}

type structuredErrorData struct {
	Error   string                 `json:"error"`
	Context map[string]interface{} `json:"context"`
}

func (h *Handler) invoke(ctx context.Context, req Request, resp *Response) {
	var input, output interface{}

	m, found := h.methods[req.Method]
	if !found {
		resp.Error = &Error{
			Code:    CodeMethodNotFound,
			Message: fmt.Sprintf("method not found: %s", req.Method),
		}

		return
	}

	if m.inputBufferType != nil {
		input = reflect.New(m.inputBufferType).Interface()
		if !h.decode(ctx, m, req, resp, input) {
			return
		}
	}

	if m.outputBufferType != nil {
		output = reflect.New(m.outputBufferType).Interface()
	}

	if err := m.useCase.Interact(ctx, input, output); err != nil {
		h.errResp(resp, "operation failed", CodeInternalError, err)

		return
	}

	h.encode(ctx, m, req, resp, output)
}

func (h *Handler) encode(ctx context.Context, m method, req Request, resp *Response, output interface{}) {
	data, err := json.Marshal(output)
	if err != nil {
		resp.Error = &Error{
			Code:    CodeInternalError,
			Message: fmt.Sprintf("failed to marshal result: %s", err.Error()),
		}

		return
	}

	if h.Validator != nil && !h.SkipResultValidation {
		if err := h.Validator.ValidateResult(req.Method, data); err != nil {
			if m.failingUseCase != nil {
				err = m.failingUseCase.Interact(context.WithValue(ctx, errCtxKey{}, err), nil, nil)
			}

			h.errResp(resp, "invalid result", CodeInternalError, err)

			return
		}
	}

	resp.Result = data
}

func (h *Handler) decode(ctx context.Context, m method, req Request, resp *Response, input interface{}) bool {
	if err := json.Unmarshal(req.Params, input); err != nil {
		if m.failingUseCase != nil {
			err = m.failingUseCase.Interact(context.WithValue(ctx, errCtxKey{}, err), nil, nil)
		}

		h.errResp(resp, "failed to unmarshal parameters", CodeInvalidParams, err)

		return false
	}

	if h.Validator != nil && !h.SkipParamsValidation {
		if err := h.Validator.ValidateParams(req.Method, req.Params); err != nil {
			if m.failingUseCase != nil {
				err = m.failingUseCase.Interact(context.WithValue(ctx, errCtxKey{}, err), nil, nil)
			}

			h.errResp(resp, "invalid parameters", CodeInvalidParams, err)

			return false
		}
	}

	return true
}

func (h *Handler) errResp(resp *Response, msg string, code ErrorCode, err error) {
	resp.Error = &Error{
		Code:    code,
		Message: msg,
	}

	var se ErrWithFields
	if errors.As(err, &se) {
		resp.Error.Data = structuredErrorData{
			Error:   se.Error(),
			Context: se.Fields(),
		}
	} else if err != nil {
		resp.Error.Data = err.Error()
	}
}

func (h *Handler) fail(w http.ResponseWriter, err error, code ErrorCode) {
	resp := Response{
		JSONRPC: ver,
		Error: &Error{
			Code:    code,
			Message: err.Error(),
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	_, err = w.Write(data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}
}
