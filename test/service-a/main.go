package main

import (
	"net/http"
)

func main() {
	mux := http.DefaultServeMux
	mux.HandleFunc("/hello1", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("this is tenant-aaaaaa with hello111111"))
	})
	mux.HandleFunc("/hello2", func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("this is tenant-aaaaaa with hello222222"))
	})

	//http.ListenAndServe(":8200", mux)
	http.ListenAndServeTLS(":8200", "conf/https_to_https/server.crt", "conf/https_to_https/server.key", mux)
}
