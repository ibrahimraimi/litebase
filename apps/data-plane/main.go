package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"litebase/apps/data-plane/internal/api"
	"litebase/apps/data-plane/internal/queue"
	"litebase/apps/data-plane/internal/sqlite"
)

func main() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)

	// Initialize the database for local dev
	dbPath := "./data/main.db"
	db, err := sqlite.Open(dbPath)
	if err != nil {
		log.Fatalf("Failed to open db: %v", err)
	}
	defer db.Close()

	// Initialize the write queue
	wq := queue.NewWriteQueue(db)
	defer wq.Close()

	// Initialize the API
	executor := api.NewSQLExecutor(db, wq)
	executor.RegisterRoutes(r)

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Litebase Data Plane Prototype\n"))
	})

	port := ":8081"
	fmt.Printf("Data Plane starting on port %s\n", port)
	log.Fatal(http.ListenAndServe(port, r))
}
