package queue

import (
	"database/sql"
	"log"
)

// WriteTask represents a task to be executed serialized
type WriteTask struct {
	Query string
	Args  []interface{}
	ResCh chan sqlResult
}

type sqlResult struct {
	Result sql.Result
	Err    error
}

// WriteQueue ensures only one write operation happens at a time for a specific DB instance.
type WriteQueue struct {
	tasks chan WriteTask
	db    DatabaseExecutor // Interface to avoid direct sqlite dependency
}

// DatabaseExecutor defines what the queue needs from a DB
type DatabaseExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// NewWriteQueue initializes a worker for managing writes sequentially.
func NewWriteQueue(db DatabaseExecutor) *WriteQueue {
	wq := &WriteQueue{
		tasks: make(chan WriteTask, 100),
		db:    db,
	}
	go wq.startWorker()
	return wq
}

func (wq *WriteQueue) startWorker() {
	for task := range wq.tasks {
		res, err := wq.db.Exec(task.Query, task.Args...)
		task.ResCh <- sqlResult{Result: res, Err: err}
	}
}

// Enqueue adds a task and waits for the result synchronously. 
func (wq *WriteQueue) Enqueue(query string, args ...interface{}) (sql.Result, error) {
	resCh := make(chan sqlResult, 1)
	wq.tasks <- WriteTask{
		Query: query,
		Args:  args,
		ResCh: resCh,
	}
	res := <-resCh
	return res.Result, res.Err
}

// Close stops the worker gracefully.
func (wq *WriteQueue) Close() {
	close(wq.tasks)
	log.Println("WriteQueue shut down")
}
