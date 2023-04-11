package main

import (
	"net/http"
)

func main() {
	mux := http.DefaultServeMux
	mux.HandleFunc("/hello1", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("this is tenant-bbbbbb with hello111111"))
	})
	mux.HandleFunc("/hello2", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("this is tenant-bbbbbb with hello222222"))
	})

	http.ListenAndServe(":8300", mux)
}
