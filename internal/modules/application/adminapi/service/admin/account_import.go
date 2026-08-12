package admin

import (
	"bytes"
	"encoding/json"
	"fmt"

	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	codexevents "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

// chatGPTAccountImportBody normalizes legacy wrapped imports and direct JSON
// credential input at the Admin HTTP boundary.
type chatGPTAccountImportBody struct {
	Tokens     []string
	Accounts   []accevents.ExportItem
	SourceType string
}

func (v *chatGPTAccountImportBody) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return fmt.Errorf("account import must be a JSON object or array")
	}
	*v = chatGPTAccountImportBody{}
	switch data[0] {
	case '[':
		tokens, accounts, err := decodeChatGPTImportItems(data)
		if err != nil {
			return err
		}
		v.Tokens, v.Accounts = tokens, accounts
		return nil
	case '{':
		var envelope struct {
			Tokens     json.RawMessage `json:"tokens"`
			Accounts   json.RawMessage `json:"accounts"`
			SourceType string          `json:"source_type"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return err
		}
		if envelope.Tokens != nil || envelope.Accounts != nil {
			if envelope.Tokens != nil {
				if err := json.Unmarshal(envelope.Tokens, &v.Tokens); err != nil {
					return fmt.Errorf("tokens must be an array of strings: %w", err)
				}
			}
			if envelope.Accounts != nil {
				accounts, err := decodeJSONObjectOrArray[accevents.ExportItem](envelope.Accounts)
				if err != nil {
					return fmt.Errorf("accounts: %w", err)
				}
				v.Accounts = accounts
			}
			v.SourceType = envelope.SourceType
			return nil
		}
		var account accevents.ExportItem
		if err := json.Unmarshal(data, &account); err != nil {
			return err
		}
		v.Accounts = []accevents.ExportItem{account}
		v.SourceType = envelope.SourceType
		return nil
	default:
		return fmt.Errorf("account import must be a JSON object or array")
	}
}

func decodeChatGPTImportItems(data []byte) ([]string, []accevents.ExportItem, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, nil, err
	}
	tokens := make([]string, 0, len(items))
	accounts := make([]accevents.ExportItem, 0, len(items))
	for _, item := range items {
		item = bytes.TrimSpace(item)
		if len(item) == 0 {
			return nil, nil, fmt.Errorf("account entry is empty")
		}
		switch item[0] {
		case '"':
			var token string
			if err := json.Unmarshal(item, &token); err != nil {
				return nil, nil, err
			}
			tokens = append(tokens, token)
		case '{':
			var account accevents.ExportItem
			if err := json.Unmarshal(item, &account); err != nil {
				return nil, nil, err
			}
			accounts = append(accounts, account)
		default:
			return nil, nil, fmt.Errorf("account entry must be a token string or credential object")
		}
	}
	return tokens, accounts, nil
}

// codexAccountImportBody accepts both the existing {"accounts":[...]} shape
// and direct credential objects or arrays.
type codexAccountImportBody struct {
	Accounts []codexevents.CredentialInput
}

func (v *codexAccountImportBody) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return fmt.Errorf("account import must be a JSON object or array")
	}
	*v = codexAccountImportBody{}
	switch data[0] {
	case '[':
		accounts, err := decodeJSONObjectOrArray[codexevents.CredentialInput](data)
		if err != nil {
			return err
		}
		v.Accounts = accounts
		return nil
	case '{':
		var envelope struct {
			Accounts json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			return err
		}
		if envelope.Accounts != nil {
			accounts, err := decodeJSONObjectOrArray[codexevents.CredentialInput](envelope.Accounts)
			if err != nil {
				return fmt.Errorf("accounts: %w", err)
			}
			v.Accounts = accounts
			return nil
		}
		var account codexevents.CredentialInput
		if err := json.Unmarshal(data, &account); err != nil {
			return err
		}
		v.Accounts = []codexevents.CredentialInput{account}
		return nil
	default:
		return fmt.Errorf("account import must be a JSON object or array")
	}
}

func decodeJSONObjectOrArray[T any](data []byte) ([]T, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("must be a JSON object or array")
	}
	switch data[0] {
	case '[':
		var values []T
		if err := json.Unmarshal(data, &values); err != nil {
			return nil, err
		}
		return values, nil
	case '{':
		var value T
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		return []T{value}, nil
	default:
		return nil, fmt.Errorf("must be a JSON object or array")
	}
}
