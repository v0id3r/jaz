package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type Account struct {
	Email string `json:"emailAddress"`
	Name  string `json:"displayName"`
}

func GetAccount(ctx context.Context, client *http.Client) (Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.googleapis.com/drive/v3/about?fields=user(emailAddress,displayName)", nil)
	if err != nil {
		return Account{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Account{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Account{}, fmt.Errorf("google Drive account verification: %s", resp.Status)
	}
	var result struct {
		User Account `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return Account{}, err
	}
	if result.User.Email == "" {
		return Account{}, fmt.Errorf("google Drive returned no account email")
	}
	return result.User, nil
}
