package herdr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

const SOCKET_FILE = "/Users/nutrina/.config/herdr/herdr.sock"
const WORKSPACE_LABEL = "wrapper:ws"

var requestId uint64 = 0

type HerdrRequest struct {
	ID     string            `json:"id"`
	Method string            `json:"method"`
	Params map[string]string `json:"params"`
}

type Workspace struct {
	WorkspaceId string `json:"workspace_id"`
	Label       string `json:"label"`
}

type WorkspaceList struct {
	Type       string      `json:"type"`
	Workspaces []Workspace `json:"workspaces"`
}

type HerdrResponseWorkspaceList struct {
	ID     string        `json:"id"`
	Result WorkspaceList `json:"result"`
	Error  *HerdrError   `json:"error"`
}

type HerdrResponseWorkspaceCreate struct {
	ID     string      `json:"id"`
	Result Workspace   `json:"result"`
	Error  *HerdrError `json:"error"`
}

type HerdrError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func getNextRequestId() string {
	requestId++
	return fmt.Sprintf("%d", requestId)
}

func CreateWorkspaceIfNotExists() (string, error) {
	var workspaceId string

	workspaceId, err := FindExistingWorkspaceId()

	if err != nil {
		return "", fmt.Errorf("Error reading herdr response!")
	} else {
		if workspaceId == "" {
			workspaceId, err = CreateNewWorkspaceId()

			if err != nil {
				return "", fmt.Errorf("Unable to create new workspace!")
			}
		}
	}

	return workspaceId, nil
}

func CreateNewWorkspaceId() (string, error) {
	conn, err := net.Dial("unix", SOCKET_FILE)
	if err != nil {
		return "", fmt.Errorf("Unable to connect to herdr unix socket!")
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	herdrRequest := HerdrRequest{ID: getNextRequestId(),
		Method: "workspace.create",
		Params: map[string]string{
			"label": WORKSPACE_LABEL,
			"cwd":   "~/Projects/crm",
		}}

	err = json.NewEncoder(conn).Encode(herdrRequest)
	if err != nil {
		fmt.Println("Error 1: ", err)
		return "", fmt.Errorf("Error sending herdr command!")
	}

	herdrResponse, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		fmt.Println("Error 2: ", err)
		return "", fmt.Errorf("Error reading herdr response!")
	}

	var response HerdrResponseWorkspaceCreate
	err = json.Unmarshal(herdrResponse, &response)
	if err != nil {
		return "", fmt.Errorf("Error decoding herdr JSON response!")
	}

	return response.Result.WorkspaceId, nil
}

func FindExistingWorkspaceId() (string, error) {
	conn, err := net.Dial("unix", SOCKET_FILE)
	if err != nil {
		return "", fmt.Errorf("Unable to connect to herdr unix socket!")
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	herdrRequest := HerdrRequest{ID: "1", Method: "workspace.list", Params: map[string]string{}}

	err = json.NewEncoder(conn).Encode(herdrRequest)
	if err != nil {
		return "", fmt.Errorf("Error sending herdr command!")
	}

	herdrResponse, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return "", fmt.Errorf("Error reading herdr response!")
	}

	var response HerdrResponseWorkspaceList
	err = json.Unmarshal(herdrResponse, &response)
	if err != nil {
		return "", fmt.Errorf("Error decoding herdr JSON response!")
	}

	var workspaceId string
	for _, ws := range response.Result.Workspaces {
		fmt.Println("check ws: ", ws.Label)
		if ws.Label == WORKSPACE_LABEL {
			workspaceId = ws.WorkspaceId
			break
		}
	}

	fmt.Println("Found ws id: ", workspaceId)

	return workspaceId, nil
}
