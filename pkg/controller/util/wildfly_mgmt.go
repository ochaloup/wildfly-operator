package util

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

var (
	// MgmtOpServerStateRead is a JSON structure for reading WFLY server
	MgmtOpServerStateRead = map[string]interface{}{
		"address":   []string{},
		"operation": "read-attribute",
		"name":      "server-state",
	}
	// MgmtOpReload is a JSON structure for reloading WFLY server
	MgmtOpReload = map[string]interface{}{
		"address":   []string{},
		"operation": "reload",
	}
	// MgmtOpTxnEnableRecoveryListener is a JSON structure for enabling txn recovery listener
	MgmtOpTxnEnableRecoveryListener = map[string]interface{}{
		"address": []string{
			"subsystem", "transactions",
		},
		"operation": "write-attribute",
		"name":      "recovery-listener",
		"value":     "true",
	}
	// MgmtOpTxnProbe is a JSON structure for probing transaction log store
	MgmtOpTxnProbe = map[string]interface{}{
		"address": []string{
			"subsystem", "transactions", "log-store", "log-store",
		},
		"operation": "probe",
	}
	// MgmtOpTxnRead is a JSON structure for reading transaction log store
	MgmtOpTxnRead = map[string]interface{}{
		"address": []string{
			"subsystem", "transactions", "log-store", "log-store",
		},
		"operation":       "read-children-resources",
		"child-type":      "transactions",
		"recursive":       "true",
		"include-runtime": "true",
	}
)

// ConvertJSONToReader converts the provided JSON data (a map)
//  to io.Reader which can be consumed by methods sending via HTTP
func convertJSONToReader(jsonData map[string]interface{}) (io.Reader, error) {
	jsonStreamBytes, err := json.Marshal(jsonData)
	if err != nil {
		return nil, fmt.Errorf("Fail to marshal JSON message %v", jsonData)
	}
	return bytes.NewBuffer(jsonStreamBytes), nil
}

// GenerateWflyMgmtHashedPassword generate the password in form to be saved under mgmt-users.properties
func GenerateWflyMgmtHashedPassword(user, password string) string {
	data := []byte(user + ":ManagementRealm:" + password)
	return fmt.Sprintf("%x", md5.Sum(data))
}

// IsMgmtOutcomeSuccesful verifies if the HTTP based management operation was succcesfull
//   it expects on arguments to get the response from the HTTP base operation (res)
//   and the body returned from the HTTP operation that is represented as a JSON
func IsMgmtOutcomeSuccesful(jsonBody map[string]interface{}) bool {
	return jsonBody["outcome"] == "success"
}

// ExecuteMgmtOp executes WildFly managemnt operation represented as a JSON
//  the execution runs over the HTTPDigest struct
//  returns the JSON as the return value from the HTTP
func ExecuteMgmtOp(httpDigest *HTTPDigest, mgmtOpJSON map[string]interface{}) (map[string]interface{}, error) {
	mgmtOpReader, err := convertJSONToReader(mgmtOpJSON)
	if err != nil {
		return nil, fmt.Errorf("Fail to parse JSON management command and convert it to io.Reader. Command %v. Cause: %v", mgmtOpJSON, err)
	}
	res, err := httpDigest.HTTPDigestPost(mgmtOpReader)
	if err != nil {
		return nil, fmt.Errorf("Failed to process remote management command %v. HTTP response on call %v. Cause: %v", mgmtOpJSON, res, err)
	}
	defer res.Body.Close()
	jsonBody, err := decodeJSONBody(res)
	if err != nil {
		return nil, fmt.Errorf("Cannot decode HTTP body to JSON. Processing HTTP response: %v of command: %v. Cause: %v", res, mgmtOpJSON, err)
	}
	return jsonBody, nil
}

// decodeJSONBody takes the HTTP Response (res) as a JSON body
//   and decods it to the form of the JSON type "native" to golang
func decodeJSONBody(res *http.Response) (map[string]interface{}, error) {
	var jsonBody map[string]interface{}
	err := json.NewDecoder(res.Body).Decode(&jsonBody)
	if err != nil {
		return nil, fmt.Errorf("Fail parse HTTP body to JSON, error: %v", err)
	}
	return jsonBody, nil
}

// ReadJSONDataByIndex iterates over the JSON object to return
//   data saved at the provided index. It returns string.
func ReadJSONDataByIndex(json interface{}, indexes ...string) string {
	jsonInProgress := json
	for _, index := range indexes {
		switch vv := jsonInProgress.(type) {
		case map[string]interface{}:
			jsonInProgress = vv[index]
		default:
			return ""
		}
	}
	switch vv := jsonInProgress.(type) {
	case string:
		return vv
	case int:
		return strconv.Itoa(vv)
	case bool:
		return strconv.FormatBool(vv)
	default:
		return ""
	}
}
