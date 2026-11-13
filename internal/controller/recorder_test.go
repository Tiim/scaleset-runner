package controller

import (
	"net/http"
	"strings"
)

type recorder struct {
	strings.Builder
	h    http.Header
	code int
}

func newRecorder() *recorder { return &recorder{h: http.Header{}} }

func (r *recorder) Header() http.Header { return r.h }
func (r *recorder) WriteHeader(c int)   { r.code = c }
