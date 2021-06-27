package jsonrpc_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/swaggest/jsonrpc"
	"github.com/swaggest/usecase"
)

func TestHandler_Add(t *testing.T) {
	cnt := 0

	h := jsonrpc.Handler{}
	h.OpenAPI = &jsonrpc.OpenAPI{}
	h.Validator = &jsonrpc.JSONSchemaValidator{}
	h.Middlewares = append(h.Middlewares, usecase.MiddlewareFunc(func(next usecase.Interactor) usecase.Interactor {
		return usecase.Interact(func(ctx context.Context, input, output interface{}) error {
			cnt++

			return next.Interact(ctx, input, output)
		})
	}))

	type inp struct {
		A string `json:"a" minLength:"3"`
		B int    `json:"b" maximum:"8"`
	}

	type outp struct {
		B int    `json:"b" maximum:"10"`
		A string `json:"a"`
	}

	u := usecase.NewIOI(new(inp), new(outp), func(ctx context.Context, input, output interface{}) error {
		in, ok := input.(*inp)
		assert.True(t, ok)

		out, ok := output.(*outp)
		assert.True(t, ok)

		out.A = in.A
		out.B = in.B

		return nil
	})
	u.SetName("echo")

	h.Add(u)

	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"jsonrpc":"2.0","method":"echo","params":{"a":"abc","b":5},"id":1}`)))
	require.NoError(t, err)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, `{"jsonrpc":"2.0","result":{"b":5,"a":"abc"},"id":1}`, w.Body.String())
	assert.Equal(t, 1, cnt)

	req, err = http.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"jsonrpc":"2.0","method":"echo","params":{"a":"abc","b":"invalid"},"id":1}`)))
	require.NoError(t, err)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, `{"jsonrpc":"2.0","error":{"code":-32602,"message":"failed to unmarshal parameters","data":"json: cannot unmarshal string into Go struct field inp.b of type int"},"id":1}`, w.Body.String())
	assert.Equal(t, 2, cnt)

	req, err = http.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"jsonrpc":"2.0","method":"echo","params":{"a":"a","b":9},"id":1}`)))
	require.NoError(t, err)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, `{"jsonrpc":"2.0","error":{"code":-32602,"message":"invalid parameters","data":{"error":"validation failed","context":{"params":["#/a: length must be \u003e= 3, but got 1","#/b: must be \u003c= 8 but found 9","#: validation failed"]}}},"id":1}`, w.Body.String())
	assert.Equal(t, 3, cnt)
}
