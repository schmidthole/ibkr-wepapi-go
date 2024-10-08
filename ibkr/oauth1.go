package ibkr

import (
	"crypto/dsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type OAuthContext interface {
	GenerateLiveSessionToken(client *http.Client, baseUrl string) error
	GetOAuthHeader(method string, requestUrl string) (string, error)
}

type IbkrOAuthCredentials struct {
	CustomerKey       string `yaml:"customer_key"`
	AccessToken       string `yaml:"access_token"`
	AccessSecret      string `yaml:"access_secret"`
	SigningKeyPath    string `yaml:"signing_key_path"`
	EncryptionKeyPath string `yaml:"encryption_key_path"`
	DHParamsPath      string `yaml:"dh_params_path"`
}

type IbkrOAuthContext struct {
	BaseUrl       string
	ConsumerKey   string
	SigningKey    *rsa.PrivateKey
	EncryptionKey *rsa.PrivateKey
	DhParams      *dsa.Parameters
	AccessToken   string
	AccessSecret  string
	LstExpiration int64
	Lst           string
}

type liveSessionTokenResponse struct {
	DhResponse    string `json:"diffie_hellman_response"`
	LstSignature  string `json:"live_session_token_signature"`
	LstExpiration int64  `json:"live_session_token_expiration"`
}

func NewIbkrOAuthContext(
	consumerKey string,
	accessToken string,
	accessSecret string,
	signingKeyPemPath string,
	encryptionKeyPemPath string,
	dhParamsPemPath string,
) (*IbkrOAuthContext, error) {
	signingKey, err := ImportRsaKeyFromPem(signingKeyPemPath)
	if err != nil {
		return nil, err
	}

	encryptionKey, err := ImportRsaKeyFromPem(encryptionKeyPemPath)
	if err != nil {
		return nil, err
	}

	dhParams, err := ImportDhParametersFromPem(dhParamsPemPath)
	if err != nil {
		return nil, err
	}

	return &IbkrOAuthContext{
		ConsumerKey:   consumerKey,
		SigningKey:    signingKey,
		EncryptionKey: encryptionKey,
		DhParams:      dhParams,
		AccessToken:   accessToken,
		AccessSecret:  accessSecret,
	}, nil
}

func NewIbkrOAuthContextFromFile(credentialsFilePath string) (*IbkrOAuthContext, error) {
	data, err := os.ReadFile(credentialsFilePath)
	if err != nil {
		return nil, err
	}

	var credentials IbkrOAuthCredentials

	err = yaml.Unmarshal(data, &credentials)
	if err != nil {
		return nil, err
	}

	return NewIbkrOAuthContext(
		credentials.CustomerKey,
		credentials.AccessToken,
		credentials.AccessSecret,
		credentials.SigningKeyPath,
		credentials.EncryptionKeyPath,
		credentials.DHParamsPath,
	)
}

func generateNonce(bitLength int) (*big.Int, error) {
	nonce, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), uint(bitLength)))
	if err != nil {
		return nil, err
	}

	return nonce, nil
}

func (i *IbkrOAuthContext) generateDhChallenge(dhRandom *big.Int) *big.Int {
	dhChallenge := big.NewInt(0)
	dhChallenge.Exp(i.DhParams.G, dhRandom, i.DhParams.P)
	return dhChallenge
}

func (i *IbkrOAuthContext) getPrepend() ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(i.AccessSecret)
	if err != nil {
		return nil, err
	}

	return rsa.DecryptPKCS1v15(rand.Reader, i.EncryptionKey, ciphertext)
}

func getOAuthTimestamp() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

func (i *IbkrOAuthContext) GetOAuthHeader(method string, requestUrl string) (string, error) {
	if i.Lst == "" {
		return "", fmt.Errorf("ibkr oauth live session token not present")
	}

	if i.LstExpiration < time.Now().Unix() {
		return "", fmt.Errorf("ibker oauth live session token likely expired")
	}

	timestamp := getOAuthTimestamp()
	params := OAuthParams{}

	nonce, err := generateNonce(128)
	if err != nil {
		return "", err
	}

	params["oauth_consumer_key"] = i.ConsumerKey
	params["oauth_nonce"] = nonce.Text(16)
	params["oauth_signature_method"] = "HMAC-SHA256"
	params["oauth_timestamp"] = timestamp
	params["oauth_token"] = i.AccessToken

	baseString := fmt.Sprintf(
		"%v&%v%v",
		method,
		url.QueryEscape(requestUrl),
		url.QueryEscape(params.ToSignatureString()),
	)

	tokenBytes, err := base64.StdEncoding.DecodeString(i.Lst)
	if err != nil {
		return "", err
	}

	h := hmac.New(sha256.New, tokenBytes)
	h.Write([]byte(baseString))
	params["oauth_signature"] = url.QueryEscape(base64.StdEncoding.EncodeToString(h.Sum(nil)))

	params["realm"] = "limited_poa"

	return params.ToHeaderString(), nil
}

func (i *IbkrOAuthContext) GenerateLiveSessionToken(client *http.Client, baseUrl string) error {
	dhRandom, err := generateNonce(256)
	if err != nil {
		return err
	}

	dhChallenge := i.generateDhChallenge(dhRandom)

	prepend, err := i.getPrepend()
	if err != nil {
		return err
	}

	nonce, err := generateNonce(128)
	if err != nil {
		return err
	}

	tokenUrl := fmt.Sprintf("%v/oauth/live_session_token", baseUrl)

	params := OAuthParams{}
	params["diffie_hellman_challenge"] = dhChallenge.Text(16)
	params["oauth_consumer_key"] = "TESTCONS" //i.ConsumerKey
	params["oauth_nonce"] = nonce.Text(16)
	params["oauth_signature_method"] = "RSA-SHA256"
	params["oauth_timestamp"] = getOAuthTimestamp()
	params["oauth_token"] = i.AccessToken

	params.logRaw()

	baseString := fmt.Sprintf(
		"%v%v&%v%v",
		hex.EncodeToString(prepend),
		methodPost,
		url.QueryEscape(tokenUrl),
		url.PathEscape(params.ToSignatureString()),
	)

	log.Printf("base string: %v", baseString)

	signature, err := SignRsa([]byte(baseString), i.SigningKey)
	if err != nil {
		return err
	}

	params["oauth_signature"] = url.QueryEscape(base64.StdEncoding.EncodeToString(signature))
	params["realm"] = "test_realm"

	req, err := http.NewRequest(methodPost, tokenUrl, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", "golang/1.23.1")
	req.Header.Set("Authorization", params.ToHeaderString())

	logRequest(req)

	rsp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer rsp.Body.Close()

	logResponse(rsp)

	if rsp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad live session token statusCode: %v", rsp.StatusCode)
	}

	body, err := io.ReadAll(rsp.Body)
	if err != nil {
		return err
	}

	var lstRsp liveSessionTokenResponse
	err = json.Unmarshal(body, &lstRsp)
	if err != nil {
		return err
	}

	dhResponse := new(big.Int)
	dhResponse.SetString(lstRsp.DhResponse, 16)

	i.LstExpiration = lstRsp.LstExpiration
	lstSignature := lstRsp.LstSignature

	kBig := big.NewInt(0)
	kBig.Exp(dhResponse, dhRandom, i.DhParams.P)
	kBytes := kBig.Bytes()

	hCalc := hmac.New(sha1.New, kBytes)
	hCalc.Write(prepend)
	lstBytes := hCalc.Sum(nil)

	i.Lst = base64.StdEncoding.EncodeToString(lstBytes)

	hVerify := hmac.New(sha1.New, lstBytes)
	hVerify.Write([]byte(i.ConsumerKey))

	verifyBytes := hVerify.Sum(nil)
	verify := base64.StdEncoding.EncodeToString(verifyBytes)

	if verify != lstSignature {
		return fmt.Errorf("lst signature mismatch. calc: %v, received: %v", verify, lstSignature)
	}

	return nil
}
