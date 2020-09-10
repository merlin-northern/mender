// Copyright 2020 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.
package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"syscall"

	"github.com/dgrijalva/jwt-go"
	"github.com/mendersoftware/mender/client"
	"github.com/mendersoftware/mender/datastore"
	dev "github.com/mendersoftware/mender/device"
	"github.com/mendersoftware/mender/store"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type AuthManager interface {
	// returns true if authorization data is current and valid
	IsAuthorized() bool
	// returns device's authorization token
	AuthToken() (client.AuthToken, error)
	// removes authentication token
	RemoveAuthToken() error
	// check if device key is available
	HasKey() bool
	// generate device key (will overwrite an already existing key)
	GenerateKey() error

	client.AuthDataMessenger
}

const (
	noAuthToken = client.EmptyAuthToken
)

type MenderAuthManager struct {
	store            store.Store
	keyStore         *store.Keystore
	idSrc            dev.IdentityDataGetter
	tenantToken      client.AuthToken
	deviceConnectUrl string
}

type AuthManagerConfig struct {
	AuthDataStore    store.Store            // authorization data store
	KeyStore         *store.Keystore        // key storage
	IdentitySource   dev.IdentityDataGetter // provider of identity data
	TenantToken      []byte                 // tenant token
	DeviceConnectUrl string
}

func NewAuthManager(conf AuthManagerConfig) AuthManager {

	if conf.KeyStore == nil || conf.IdentitySource == nil ||
		conf.AuthDataStore == nil {
		return nil
	}

	mgr := &MenderAuthManager{
		store:            conf.AuthDataStore,
		keyStore:         conf.KeyStore,
		idSrc:            conf.IdentitySource,
		tenantToken:      client.AuthToken(conf.TenantToken),
		deviceConnectUrl: conf.DeviceConnectUrl,
	}

	if err := mgr.keyStore.Load(); err != nil && !store.IsNoKeys(err) {
		log.Errorf("Failed to load device keys: %v", err)
		// Otherwise ignore error returned from Load() call. It will
		// just result in an empty keyStore which in turn will cause
		// regeneration of keys.
	}

	return mgr
}

func (m *MenderAuthManager) IsAuthorized() bool {
	adata, err := m.AuthToken()
	if err != nil {
		return false
	}

	if adata == noAuthToken {
		return false
	}

	return true
}

func (m *MenderAuthManager) MakeAuthRequest() (*client.AuthRequest, error) {

	var err error
	authd := client.AuthReqData{}

	idata, err := m.idSrc.Get()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to obtain identity data")
	}

	authd.IdData = idata

	// fill device public key
	authd.Pubkey, err = m.keyStore.PublicPEM()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to obtain device public key")
	}

	tentok := strings.TrimSpace(string(m.tenantToken))

	log.Debugf("Tenant token: %s", tentok)

	// fill tenant token
	authd.TenantToken = string(tentok)

	log.Debugf("Authorization data: %v", authd)

	reqdata, err := authd.ToBytes()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to convert auth request data")
	}

	// generate signature
	sig, err := m.keyStore.Sign(reqdata)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to sign auth request")
	}

	return &client.AuthRequest{
		Data:      reqdata,
		Token:     client.AuthToken(tentok),
		Signature: sig,
	}, nil
}

type TenantMC struct {
	TenantId string `json:"tenant_id"`
}

type DeviceMC struct {
	DeviceId string `json:"device_id"`
}

func (m *MenderAuthManager) RecvAuthResponse(data []byte) error {
	if len(data) == 0 {
		return errors.New("empty auth response data")
	}

	log.Infof("RecvAuthResponse got token: '%s'", string(data))
	parser := jwt.Parser{}
	claims := jwt.MapClaims{}
	_, _, _ = parser.ParseUnverified(string(data), &claims)

	if err := m.store.WriteAll(datastore.AuthTokenName, data); err != nil {
		return errors.Wrapf(err, "failed to save auth token")
	}

	t := TenantMC{TenantId: claims["mender.tenant"].(string)}
	d := DeviceMC{DeviceId: claims["sub"].(string)}
	log.Infof("MC: running POST '%v'", t)
	providesBody, err := json.Marshal(t)

	r := bytes.NewBuffer(providesBody)
	postReq, err := http.NewRequest(http.MethodPost, "http://"+m.deviceConnectUrl+"/api/internal/v1/deviceconnect/tenants", r)
	postReq.Header.Add("Content-Type", "application/json")
	client := &http.Client{}
	response, err := client.Do(postReq)
	log.Infof("MC: POST %v : %v,%v", t, response, err)

	log.Infof("MC: running POST '%v'", d)
	providesBody, err = json.Marshal(d)
	r = bytes.NewBuffer(providesBody)
	postReq, err = http.NewRequest(http.MethodPost, "http://"+m.deviceConnectUrl+"/api/internal/v1/deviceconnect/tenants/5abcb6de7a673a0001287c71/devices", r)
	postReq.Header.Add("Content-Type", "application/json")
	client = &http.Client{}
	response, err = client.Do(postReq)
	log.Infof("MC: POST %v : %v,%v", d, response, err)

	pid := os.Getpid()
	log.Infof("MC: RecvAuthResponse pid: %d", pid)
	p, err := os.FindProcess(pid)
	log.Infof("MC: RecvAuthResponse p,err %v,%v", p, err)
	err = p.Signal(syscall.SIGUSR2)
	log.Infof("MC: RecvAuthResponse since we got auth request response we sent the signal to start. ('%v')", err)
	return nil
}

func (m *MenderAuthManager) AuthToken() (client.AuthToken, error) {
	data, err := m.store.ReadAll(datastore.AuthTokenName)
	if err != nil {
		if os.IsNotExist(err) {
			return noAuthToken, nil
		}
		return noAuthToken, errors.Wrapf(err, "failed to read auth token data")
	}

	return client.AuthToken(data), nil
}

func (m *MenderAuthManager) RemoveAuthToken() error {
	// remove token only if we have one
	if aToken, err := m.AuthToken(); err == nil && aToken != noAuthToken {
		return m.store.Remove(datastore.AuthTokenName)
	}
	return nil
}

func (m *MenderAuthManager) HasKey() bool {
	return m.keyStore.Private() != nil
}

func (m *MenderAuthManager) GenerateKey() error {
	if err := m.keyStore.Generate(); err != nil {
		if store.IsStaticKey(err) {
			return err
		}
		log.Errorf("Failed to generate device key: %v", err)
		return errors.Wrapf(err, "failed to generate device key")
	}

	if err := m.keyStore.Save(); err != nil {
		log.Errorf("Failed to save device key: %s", err)
		return NewFatalError(err)
	}
	return nil
}
