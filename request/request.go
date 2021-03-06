package request

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/caiyeon/goldfish/vault"
	"github.com/gorilla/securecookie"
	"github.com/hashicorp/go-uuid"
	"github.com/hashicorp/vault/helper/xor"
	"github.com/mitchellh/hashstructure"
	"github.com/mitchellh/mapstructure"
	"github.com/fatih/structs"

	"golang.org/x/sync/syncmap"
)

// operations on the same request should not interweave,
// a map of string to string (hash) will prevent this race condition
var lockMap syncmap.Map

// only one goroutine should perform vault root generation at a time
var lockRoot sync.Mutex

type Request interface {
	IsRootOnly() bool
	Verify(*vault.AuthInfo) error
	Approve(string, string) error
	Reject(*vault.AuthInfo, string) error
	Create(*vault.AuthInfo, map[string]interface{}) (string, error)
}

// adds a request if user has authentication
func Add(auth *vault.AuthInfo, raw map[string]interface{}) (string, error) {
	t := ""
	if typeRaw, ok := raw["Type"]; ok {
		t, ok = typeRaw.(string)
	}
	if t == "" {
		return "", errors.New("Type field is empty")
	}

	switch strings.ToLower(t) {
	case "policy":
		var req PolicyRequest

		// construct request fields
		hash, err := req.Create(auth, raw)
		if err != nil {
			return "", err
		}

		// lock hash in map before writing to vault cubbyhole
		_, loaded := lockMap.LoadOrStore(hash, true)
		if loaded {
			return "", errors.New("Someone else is currently editing this request")
		}
		defer lockMap.Delete(hash)

		_, err = vault.WriteToCubbyhole("requests/" + hash, structs.Map(req))
		return hash, err

	default:
		return "", errors.New("Unsupported request type")
	}
}

// fetches a request if it exists, and if user has authentication
func Get(auth *vault.AuthInfo, hash string) (Request, error) {
	_, loaded := lockMap.LoadOrStore(hash, true)
	if loaded {
		return nil, errors.New("Someone else is currently editing this request")
	}
	defer lockMap.Delete(hash)

	// fetch request from cubbyhole
	resp, err := vault.ReadFromCubbyhole("requests/" + hash)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("Change ID not found")
	}

	// decode secret to a request
	t := ""
	if raw, ok := resp.Data["Type"]; ok {
		t, ok = raw.(string)
	}
	if t == "" {
		return nil, errors.New("Invalid request type")
	}

	switch strings.ToLower(t) {
	case "policy":
		// decode secret into policy request
		var req PolicyRequest
		if err := mapstructure.Decode(resp.Data, &req); err != nil {
			return nil, err
		}
		// verify hash
		hash_uint64, err := hashstructure.Hash(req, nil)
		if err != nil || strconv.FormatUint(hash_uint64, 16) != hash {
			return nil, errors.New("Hashes do not match")
		}
		// verify policy request is still valid
		if err := req.Verify(auth); err != nil {
			return nil, err
		}
		return &req, nil

	default:
		return nil, errors.New("Invalid request type: " + t)
	}
}

// if unseal is nonempty string, approve request with current auth
// otherwise, add unseal to list of unseals to generate root token later
func Approve(auth *vault.AuthInfo, hash string, unseal string) error {
	_, loaded := lockMap.LoadOrStore(hash, true)
	if loaded {
		return errors.New("Someone else is currently editing this request")
	}
	defer lockMap.Delete(hash)

	// fetch request from cubbyhole
	resp, err := vault.ReadFromCubbyhole("requests/" + hash)
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("Change ID not found")
	}

	// decode secret to a request
	t := ""
	if raw, ok := resp.Data["Type"]; ok {
		t, ok = raw.(string)
	}
	if t == "" {
		return errors.New("Invalid request type")
	}

	switch strings.ToLower(t) {
	case "policy":
		// decode secret into policy request
		var req PolicyRequest
		if err := mapstructure.Decode(resp.Data, &req); err != nil {
			return err
		}
		// verify hash
		hash_uint64, err := hashstructure.Hash(req, nil)
		if err != nil || strconv.FormatUint(hash_uint64, 16) != hash {
			return errors.New("Hashes do not match")
		}
		// verify policy request is still valid
		if err := req.Verify(auth); err != nil {
			return err
		}
		return req.Approve(hash, unseal)

	default:
		return errors.New("Invalid request type: " + t)
	}
}

// deletes request, if user is authorized to read resource
func Reject(auth *vault.AuthInfo, hash string) error {
	_, loaded := lockMap.LoadOrStore(hash, true)
	if loaded {
		return errors.New("Someone else is currently editing this request")
	}
	defer lockMap.Delete(hash)

	// fetch request from cubbyhole
	resp, err := vault.ReadFromCubbyhole("requests/" + hash)
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("Change ID not found")
	}

	// decode secret to a request
	t := ""
	if raw, ok := resp.Data["Type"]; ok {
		t, ok = raw.(string)
	}
	if t == "" {
		return errors.New("Invalid request type")
	}

	// verify user can access resource
	switch strings.ToLower(t) {
	case "policy":
		// decode secret into policy request
		var req PolicyRequest
		if err := mapstructure.Decode(resp.Data, &req); err != nil {
			return err
		}
		// verify hash
		hash_uint64, err := hashstructure.Hash(req, nil)
		if err != nil || strconv.FormatUint(hash_uint64, 16) != hash {
			return errors.New("Hashes do not match")
		}
		// verify policy request is still valid
		return req.Reject(auth, hash)

	default:
		return errors.New("Invalid request type: " + t)
	}
}

func IsRootOnly(req Request) bool {
	return req.IsRootOnly()
}

// attempts to generate a root token via unseal keys
// will return error if another key generation process is underway
func generateRootToken(unsealKeys []string) (string, error) {
	lockRoot.Lock()
	defer lockRoot.Unlock()

	otp := base64.StdEncoding.EncodeToString(securecookie.GenerateRandomKey(16))
	status, err := vault.GenerateRootInit(otp)
	if err != nil {
		return "", err
	}

	if status.EncodedRootToken == "" {
		for _, s := range unsealKeys {
			status, err = vault.GenerateRootUpdate(s, status.Nonce)
			// an error likely means one of the unseals was not valid
			if err != nil {
				errS := "Could not generate root token: " + err.Error()
				// try to cancel the root generation
				if err := vault.GenerateRootCancel(); err != nil {
					errS += ". Attempted to cancel root generation, but: " + err.Error()
				}
				return "", errors.New(errS)
			}
		}
	}

	if status.EncodedRootToken == "" {
		return "", errors.New("Could not generate root token. Was vault re-keyed just now?")
	}

	tokenBytes, err := xor.XORBase64(status.EncodedRootToken, otp)
	if err != nil {
		return "", errors.New("Could not decode root token. Please search and revoke")
	}

	token, err := uuid.FormatUUID(tokenBytes)
	if err != nil {
		return "", errors.New("Could not decode root token. Please search and revoke")
	}

	return token, nil
}

// writes the provided unseal in and returns a slice of all unseals in hash
func appendUnseal(hash, unseal string) ([]string, error) {
	// read current request from cubbyhole
	resp, err := vault.ReadFromCubbyhole("unseal_wrapping_tokens/" + hash)
	if err != nil {
		return nil, err
	}

	var wrappingTokens []string

	// if there are already unseals, read them and append
	if resp != nil {
		raw := ""
		if temp, ok := resp.Data["wrapping_tokens"]; ok {
			raw, _ = temp.(string)
		}
		if raw == "" {
			return nil, errors.New("Could not find key 'wrapping_tokens' in cubbyhole")
		}
		wrappingTokens = append(wrappingTokens, strings.Split(raw, ";")...)
	}

	// wrap the unseal token
	newWrappingToken, err := vault.WrapData("60m", map[string]interface{}{
		"unseal_token": unseal,
	})
	if err != nil {
		return nil, err
	}

	// add the new unseal key in
	wrappingTokens = append(wrappingTokens, newWrappingToken)

	// write the unseals back to the cubbyhole
	_, err = vault.WriteToCubbyhole("unseal_wrapping_tokens/"+hash,
		map[string]interface{}{
			"wrapping_tokens": strings.Trim(strings.Join(strings.Fields(fmt.Sprint(wrappingTokens)), ";"), "[]"),
		},
	)
	return wrappingTokens, err
}

func unwrapUnseals(wrappingTokens []string) (unseals []string, err error) {
	for _, wrappingToken := range wrappingTokens {
		data, err := vault.UnwrapData(wrappingToken)
		if err != nil {
			return nil, err
		}
		if unseal, ok := data["unseal_token"]; ok {
			unseals = append(unseals, unseal.(string))
		} else {
			return nil, errors.New("One of the wrapping tokens timed out. Progress reset.")
		}
	}
	return
}
