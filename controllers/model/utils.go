package model

import (
	"crypto/rand"
	"encoding/base64"
)

func generateRandomBytes(n int) []byte {
	b := make([]byte, n)
	_, err := rand.Read(b)
	if err != nil {
		panic(err)
	}
	return b
}

func RandStringRunes(s int) string {
	b := generateRandomBytes(s)
	return base64.URLEncoding.EncodeToString(b)
}

func MergeAnnotations(requested map[string]string, existing map[string]string) map[string]string {
	if existing == nil {
		return requested
	}

	for k, v := range requested {
		existing[k] = v
	}
	return existing
}

type GrafanaKeyBody struct {
	Name          string `json:"name"`
	Role          string `json:"role"`
	SecondsToLive int    `json:"secondsToLive"`
}
type Grafana_key struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}
type Grafana_user struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Login    string `json:"login"`
	Password string `json:"password"`
}

var GrafanaKey string
var HyperauthUrl string

type Grafana_User_Get struct {
	Id             int    `json:"id"`
	Email          string `json:"email"`
	Name           string `json:"name"`
	Login          string `json:"login"`
	Theme          string `json:"light"`
	OrgId          string `json:"OrgId"`
	IsGrafanaAdmin string `json:"isGrafanaAdmin"`
	IsDisabled     string `json:"isDisabled"`
	IsExternal     string `json:"isExternal"`
	AuthLabels     string `json:"authLabels"`
	UpdatedAt      string `json:"updatedAt"`
	CreatedAt      string `json:"createdAt"`
	AvatarUrl      string `json:"avatarUrl"`
}

type Grafana_Dashboad_resp struct {
	Meta      string                     `json:"meta"`
	Dashboard Grafana_Dashboad_dashboard `json:"dashboard"`
}

type Grafana_Dashboad_dashboard struct {
	Id      int    `json:"id"`
	Uid     string `json:"uid"`
	Url     string `json:"url"`
	Status  string `json:"status"`
	Version string `json:"version"`
	Slug    string `json:"slug"`
}
