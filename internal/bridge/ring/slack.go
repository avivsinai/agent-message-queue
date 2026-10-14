package ring

import (
	"encoding/json"
	"io"
)

const SlackText = "AMQ mail waiting."

type slackBody struct {
	Text string `json:"text"`
}

func RingSlack(keyFile, handle, pendingText string, client Poster, stderr io.Writer) error {
	if _, err := parseRingArgs(handle, pendingText); err != nil {
		return err
	}
	var creds slackFile
	if err := readKeyFile(keyFile, &creds); err != nil {
		return err
	}
	rawURL, err := parseWebhookURL(creds.URL)
	if err != nil {
		return err
	}
	body, err := json.Marshal(slackBody{Text: SlackText})
	if err != nil {
		return err
	}
	if err := postJSON(client, rawURL, "", body); err != nil {
		writeProgress(stderr, ProgressDeferred)
		return err
	}
	writeProgress(stderr, ProgressAccepted)
	return nil
}
