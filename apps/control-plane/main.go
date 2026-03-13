package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func main() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Litebase Control Plane Prototype\n"))
	})

	port := ":8080"
	fmt.Printf("Control Plane starting on port %s\n", port)
	log.Fatal(http.ListenAndServe(port, r))
}
