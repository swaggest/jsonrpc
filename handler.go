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

type ErrorCode int

const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

type Handler struct {
	OpenAPI *OpenAPI

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

func (h *Handler) Add(u usecase.Interactor) {
	if h.methods == nil {
		h.methods = make(map[string]method)
	}

	var (
		withName usecase.HasName
	)

	if !usecase.As(u, &withName) {
		panic("use case name is required")
	}

	m := method{
		useCase: u,
	}
	m.setupInputBuffer()
	m.setupOutputBuffer()

	h.methods[withName.Name()] = m

	if h.OpenAPI != nil {
		err := h.OpenAPI.Collect(withName.Name(), u)
		if err != nil {
			panic(fmt.Sprintf("failed to add to OpenAPI schema: %s", err.Error()))
		}
	}
}

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      *interface{}    `json:"id,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	ID      *interface{}    `json:"id"`
}

type Error struct {
	Code    ErrorCode    `json:"code"`
	Message string       `json:"message"`
	Data    *interface{} `json:"data,omitemptys"`
}

var (
	errEmptyBody = errors.New("empty body")
)

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
			var resp Response

			if req.ID != nil {
				resp.ID = req.ID
				resps = append(resps, &resp)
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

		return
	} else {
		var (
			req  Request
			resp Response
		)
		if err := json.Unmarshal(reqBody, &req); err != nil {
			h.fail(w, fmt.Errorf("failed to unmarshal request: %w", err), CodeParseError)

			return
		}

		resp.ID = req.ID
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
}

func (h *Handler) invoke(ctx context.Context, req Request, resp *Response) {
	var (
		input, output interface{}
	)

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
		if err := json.Unmarshal(req.Params, input); err != nil {
			resp.Error = &Error{
				Code:    CodeInvalidParams,
				Message: fmt.Sprintf("failed to unmarshal parameters: %s", err.Error()),
			}

			return
		}
	}

	if m.outputBufferType != nil {
		output = reflect.New(m.outputBufferType).Interface()
	}

	if err := m.useCase.Interact(ctx, input, output); err != nil {
		resp.Error = &Error{
			Code:    CodeInternalError,
			Message: err.Error(),
		}

		return
	}

	if data, err := json.Marshal(output); err != nil {
		resp.Error = &Error{
			Code:    CodeInternalError,
			Message: fmt.Sprintf("failed to marshal result: %s", err.Error()),
		}

		return
	} else {
		resp.Result = data
	}
}

func (h *Handler) fail(w http.ResponseWriter, err error, code ErrorCode) {
	resp := Response{
		JSONRPC: "2.0",
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
