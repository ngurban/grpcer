// Copyright 2017 Tamás Gulácsi
//
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package grpcer

import (
	"io"
	"io/ioutil"
	"os"
	"reflect"
	"strings"

	jsoniter "github.com/json-iterator/go"
	"github.com/pkg/errors"
)

var errNewField = errors.New("new field")

type streamEncoder interface {
	WriteField(w io.Writer, name string) error
}

func mergeStreams(w io.Writer, first interface{}, recv interface {
	Recv() (interface{}, error)
},
	Log func(...interface{}) error,
) {
	if Log == nil {
		Log = func(...interface{}) error { return nil }
	}

	slice, notSlice := sliceFields(first)
	if len(slice) == 0 {
		var err error
		part := first
		enc := jsoniter.NewEncoder(w)
		for {
			if err := enc.Encode(part); err != nil {
				Log("encode", part, "error", err)
				return
			}

			part, err = recv.Recv()
			if err != nil {
				if err != io.EOF {
					Log("msg", "recv", "error", err)
				}
				break
			}
		}
		return
	}

	names := make(map[string]bool, len(slice)+len(notSlice))

	Log("slices", slice)
	w.Write([]byte("{"))
	for _, f := range notSlice {
		tw := newTrimWriter(w, "", "\n")
		jsoniter.NewEncoder(tw).Encode(f.JSONName)
		tw.Close()
		w.Write([]byte{':'})
		tw = newTrimWriter(w, "", "\n")
		jsoniter.NewEncoder(tw).Encode(f.Value)
		tw.Close()
		w.Write([]byte{','})

		names[f.Name] = false
	}
	tw := newTrimWriter(w, "", "\n")
	jsoniter.NewEncoder(tw).Encode(slice[0].JSONName)
	tw.Close()
	w.Write([]byte(":"))
	tw = newTrimWriter(w, "", "]")
	jsoniter.NewEncoder(tw).Encode(slice[0].Value)
	tw.Close()

	names[slice[0].Name] = true

	files := make(map[string]*os.File, len(slice)-1)
	for _, f := range slice[1:] {
		fh, err := ioutil.TempFile("", "merge-"+f.Name+"-")
		if err != nil {
			Log("tempFile", f.Name, "error", err)
			return
		}
		os.Remove(fh.Name())
		defer fh.Close()
		files[f.Name] = fh
		tw := newTrimWriter(fh, "", "\n")
		jsoniter.NewEncoder(tw).Encode(f.JSONName)
		tw.Close()
		io.WriteString(fh, ":[")
		tw = newTrimWriter(fh, "[", "]")
		jsoniter.NewEncoder(tw).Encode(f.Value)
		tw.Close()

		names[f.Name] = true
	}

	var part interface{}
	var err error
	for {
		part, err = recv.Recv()
		if err != nil {
			if err != io.EOF {
				Log("msg", "recv", "error", err)
			}
			break
		}

		S, nS := sliceFields(part)
		for _, f := range S {
			if isSlice, ok := names[f.Name]; !(ok && isSlice) {
				err = errors.Wrap(errNewField, f.Name)
			}
		}
		for _, f := range nS {
			if isSlice, ok := names[f.Name]; !(ok && !isSlice) {
				err = errors.Wrap(errNewField, f.Name)
			}
		}
		if err != nil {
			Log("error", err)
			//TODO(tgulacsi): close the merge and send as is
		}

		if S[0].Name == slice[0].Name {
			w.Write([]byte{','})
			tw := newTrimWriter(w, "[", "]")
			jsoniter.NewEncoder(tw).Encode(S[0].Value)
			tw.Close()
			S = S[1:]
		}
		for _, f := range S {
			fh := files[f.Name]
			if _, err := fh.Write([]byte{','}); err != nil {
				Log("write", fh.Name(), "error", err)
			}
			tw := newTrimWriter(fh, "[", "]")
			jsoniter.NewEncoder(tw).Encode(f.Value)
			tw.Close()
		}
	}
	w.Write([]byte("]"))

	for _, fh := range files {
		if _, err := fh.Seek(0, 0); err != nil {
			Log("Seek", fh.Name(), "error", err)
			continue
		}
		w.Write([]byte{','})
		io.Copy(w, fh)
		w.Write([]byte{']'})
	}
	w.Write([]byte{'}', '\n'})
}

type field struct {
	Name     string
	JSONName string
	Value    interface{}
}

func sliceFields(part interface{}) (slice, notSlice []field) {
	rv := reflect.ValueOf(part)
	t := rv.Type()
	if t.Kind() == reflect.Ptr {
		rv = rv.Elem()
		t = rv.Type()
	}
	n := t.NumField()
	for i := 0; i < n; i++ {
		f := rv.Field(i)
		tf := t.Field(i)
		fld := field{Name: tf.Name, Value: f.Interface()}
		fld.JSONName = tf.Tag.Get("json")
		if i := strings.IndexByte(fld.JSONName, ','); i >= 0 {
			fld.JSONName = fld.JSONName[:i]
		}
		if fld.JSONName == "" {
			fld.JSONName = fld.Name
		}

		if f.Type().Kind() != reflect.Slice {
			notSlice = append(notSlice, fld)
			continue
		}
		if f.IsNil() {
			continue
		}
		slice = append(slice, fld)
	}
	return slice, notSlice
}

type trimWriter struct {
	w              io.Writer
	prefix, suffix string
	buf            []byte
}

func newTrimWriter(w io.Writer, prefix, suffix string) *trimWriter {
	return &trimWriter{w: w, prefix: prefix, suffix: suffix}
}
func (tw *trimWriter) Write(p []byte) (int, error) {
	n := len(p)
	if tw.prefix != "" {
		if len(tw.prefix) >= len(p) {
			tw.prefix = tw.prefix[len(p):]
			return n, nil
		}
		p = p[len(tw.prefix):]
		tw.prefix = ""
	}

	if len(tw.buf) > 0 && len(tw.buf) >= len(tw.suffix) {
		if _, err := tw.w.Write(tw.buf); err != nil {
			return 0, err
		}
		tw.buf = tw.buf[:0]
	}
	if len(p) <= len(tw.suffix) {
		tw.buf = append(tw.buf, p...)
		return n, nil
	}
	i := len(p) - len(tw.suffix) + len(tw.buf)
	tw.buf = append(tw.buf, p[i:]...)
	_, err := tw.w.Write(p[:i])
	return n, err
}
func (tw *trimWriter) Close() error {
	if tw.suffix == string(tw.buf) {
		return nil
	}
	_, err := tw.w.Write(tw.buf)
	return err
}
