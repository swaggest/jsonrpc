package main

import (
	"context"
	"github.com/go-chi/chi/v5"
	"github.com/swaggest/jsonrpc"
	"github.com/swaggest/swgui/v3cdn"
	"github.com/swaggest/usecase"
	"log"
	"net/http"
)

func main() {
	h := &jsonrpc.Handler{}
	h.OpenAPI = &jsonrpc.OpenAPI{}

	type inp struct {
		Name string `json:"name"`
	}

	type out struct {
		Len int `json:"len"`
	}

	u := usecase.NewIOI(new(inp), new(out), func(ctx context.Context, input, output interface{}) error {
		output.(*out).Len = len(input.(*inp).Name)

		return nil
	})
	u.SetName("nameLength")

	h.Add(u)

	r := chi.NewRouter()

	r.Mount("/rpc", h)

	// Swagger UI endpoint at /docs.
	r.Method(http.MethodGet, "/docs/openapi.json", h.OpenAPI)
	r.Mount("/docs", v3cdn.NewHandler(h.OpenAPI.Reflector().Spec.Info.Title,
		"/docs/openapi.json", "/docs"))

	// Start server.
	log.Println("http://localhost:8011/docs")
	if err := http.ListenAndServe(":8011", r); err != nil {
		log.Fatal(err)
	}
}
