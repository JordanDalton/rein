package main

import (
	"context"
	"fmt"
	"net/http"
)

func cmdApproval(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return fmt.Errorf("usage: rein approval list")
	}
	p, err := loadCloudProfile()
	if err != nil || p == nil {
		return fmt.Errorf("not connected to Rein Control — run `rein login`")
	}
	token, err := loadCloudCredential(p.ControlURL)
	if err != nil {
		return err
	}
	var result struct {
		Data []struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Caller  string `json:"caller"`
			Created string `json:"created_at"`
		} `json:"data"`
	}
	if err := cloudJSON(ctx, http.MethodGet, p.ControlURL+"/api/v1/rein/approvals", token, nil, &result); err != nil {
		return err
	}
	if len(result.Data) == 0 {
		fmt.Println("No approval requests.")
		return nil
	}
	for _, approval := range result.Data {
		fmt.Printf("%s\t%s\t%s\t%s\n", approval.ID, approval.Status, approval.Caller, approval.Created)
	}
	return nil
}
