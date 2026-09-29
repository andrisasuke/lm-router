package proxy

import (
	"io"
	"sync"
)

func (s *Server) beginAccountActivity(accountID string) func() {
	if s.onAccountActivity == nil || accountID == "" {
		return func() {}
	}
	s.onAccountActivity(accountID, true)
	var once sync.Once
	return func() {
		once.Do(func() {
			s.onAccountActivity(accountID, false)
		})
	}
}

func trackAccountCall[T any](s *Server, accountID string, call func() (T, error)) (T, error) {
	done := s.beginAccountActivity(accountID)
	defer done()
	return call()
}

type activityReadCloser struct {
	io.ReadCloser
	done func()
}

func (r *activityReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.done()
	return err
}

func trackAccountStream(body io.ReadCloser, done func()) io.ReadCloser {
	if body == nil {
		done()
		return nil
	}
	return &activityReadCloser{ReadCloser: body, done: done}
}
