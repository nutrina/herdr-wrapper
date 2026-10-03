package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/nutrina/herdr-wrapper/internal/herdr"
)

type Response struct {
	Msg   string  `json:"msg"`
	Error *string `json:"error,omitempty"`
}

type TaskRequest struct {
	Description string `json:"description"`
}

func run_agent(w http.ResponseWriter, r *http.Request) {

	var ret map[string]any
	w.Header().Set("Content-Type", "application/json")
	var req TaskRequest
	err := json.NewDecoder(r.Body).Decode(&req)

	if err != nil {
		ret = map[string]any{
			"error": fmt.Sprintf("Error decoding JSON (%s)", err),
		}
	} else {
		workspaceId, err := herdr.CreateWorkspaceIfNotExists()
		if err != nil {
			ret = map[string]any{
				"error": fmt.Sprintf("Error decoding JSON (%s)", err),
			}
		} else {
			fmt.Println("WorkspaceID: ", workspaceId)
			ret = map[string]any{
				"msg": "Success!",
			}
		}
	}

	json.NewEncoder(w).Encode(ret)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent", run_agent)

	log.Println("Listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
