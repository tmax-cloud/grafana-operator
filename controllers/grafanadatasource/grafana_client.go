package grafanadatasource

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/grafana-operator/grafana-operator/v4/api/integreatly/v1alpha1"
)

const (
	DeleteDatasourceByUIDUrl = "%v/api/datasources/uid/%v"
	CreateDatasourceUrl      = "%v/api/datasources"
	GetDatasourceURL         = "%v/api/datasources/id/%v"
	GetDatasourceListURL     = "%v/api/datasources/"
	UpdateDatasourceUrl      = "%v/api/datasources/uid/%v"
)

type GrafanaClient interface {
	GetDatasourceList() ([]GrafanaDatasourceResponse, error)
	GetDatasource(UID string) (GrafanaDatasourceResponse, error)
	CreateGrafanaDatasource(datasource v1alpha1.GrafanaDataSource) (GrafanaDatasourceResponse, error)
	DeleteDatasourceByUID(UID string) (GrafanaDatasourceResponse, error)
}

func NewDataSourcesState2() *GrafanaClientImpl {
	return &GrafanaClientImpl{}
}

type GrafanaDatasourceResponse struct {
	ID       *uint   `json:"id"`
	UID      *string `json:"uid"`
	OrgID    *uint   `json:"orgId"`
	Name     *string `json:"name"`
	Type     *string `json:"type"`
	TypeName *string `json:"typeName"`
	Access   *string `json:"access"`
	URL      *string `json:"url"`
}

type GrafanaDatasourceRequest struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	TypeName string `json:"typeName"`
	Access   string `json:"access"`
	URL      string `json:"url"`
}

type GrafanaClientImpl struct {
	url      string
	user     string
	password string
	client   *http.Client
	//ClusterDataSources *v1alpha1.GrafanaDataSourceList
	//KnownDataSources   *v1alpha1.GrafanaDataSourceList
}

func newResponse() GrafanaDatasourceResponse {
	var id uint = 0
	var orgID uint = 0
	var typeName string
	var type1 string
	var uid string
	var name string
	var url string

	return GrafanaDatasourceResponse{
		ID:       &id,
		OrgID:    &orgID,
		Name:     &name,
		UID:      &uid,
		URL:      &url,
		Type:     &type1,
		TypeName: &typeName,
	}
}

func setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "grafana-operator")
}

func (r *GrafanaClientImpl) GetDatasourceList() ([]GrafanaDatasourceResponse, error) {
	rawURL := fmt.Sprintf(GetDatasourceListURL, r.url)
	parsed, err := url.Parse(rawURL)

	if err != nil {
		return nil, err
	}

	parsed.User = url.UserPassword(r.user, r.password)
	log.V(1).Info(parsed.User.String())
	req, err := http.NewRequest("GET", parsed.String(), nil)

	if err != nil {
		return nil, err
	}

	setHeaders(req)
	reqdump, err := httputil.DumpRequestOut(req, true)
	log.V(1).Info("check nil" + string(reqdump))
	var resp *http.Response
	resp, err = r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		// Grafana might be unavailable, no reason to panic, other checks are in place
		if resp.StatusCode == 503 {
			return nil, nil
		} else {
			return nil, fmt.Errorf(
				"error getting folders, expected status 200 but got %v",
				resp.StatusCode)
		}
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var datasrouces []GrafanaDatasourceResponse
	err = json.Unmarshal(data, &datasrouces)

	return datasrouces, err
}

func (r *GrafanaClientImpl) GetDatasource(UID string) (GrafanaDatasourceResponse, error) {
	rawURL := fmt.Sprintf(GetDatasourceURL, r.url, UID)
	response := newResponse()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return response, err
	}

	parsed.User = url.UserPassword(r.user, r.password)
	req, err := http.NewRequest("GET", parsed.String(), nil)

	if err != nil {
		return response, err
	}

	setHeaders(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return response, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return response, err
	} else if resp.StatusCode != 200 {
		return response, fmt.Errorf(
			"error searching for dashboard, expected status 200 but got %v",
			resp.StatusCode)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return response, err
	}

	err = json.Unmarshal(data, &response)

	return response, err
}

func (r *GrafanaClientImpl) CreateGrafanaDatasource(datasource v1alpha1.GrafanaDataSource) (GrafanaDatasourceResponse, error) {
	rawURL := fmt.Sprintf(CreateDatasourceUrl, r.url)
	response := newResponse()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return response, err
	}
	//log.V(1).Info("URL : ", datasource.Spec.Datasources[0].Access)
	raw, err := json.Marshal(GrafanaDatasourceRequest{
		Name: datasource.Spec.Datasources[0].Name,

		Type: datasource.Spec.Datasources[0].Type,

		URL:    datasource.Spec.Datasources[0].Url,
		Access: datasource.Spec.Datasources[0].Access,
	})
	if err != nil {
		return response, err
	}

	parsed.User = url.UserPassword(r.user, r.password)
	req, err := http.NewRequest("POST", parsed.String(), bytes.NewBuffer(raw))

	if err != nil {
		return response, err
	}

	setHeaders(req)
	resp, err := r.client.Do(req)
	if err != nil {
		return response, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 503 {
		return response, fmt.Errorf(
			"error creating dashboard, expected status 200 but got %v",
			resp.StatusCode)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return response, err
	}
	err = json.Unmarshal(data, &response)

	return response, err
}

func (r *GrafanaClientImpl) DeleteDatasourceByUID(UID string) (GrafanaDatasourceResponse, error) {
	rawURL := fmt.Sprintf(DeleteDatasourceByUIDUrl, r.url, UID)
	response := newResponse()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return response, err
	}

	parsed.User = url.UserPassword(r.user, r.password)
	req, err := http.NewRequest("DELETE", parsed.String(), nil)

	if err != nil {
		return response, err
	}

	setHeaders(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return response, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return response, fmt.Errorf(
			"error deleting datasrouce, expected status 200 but got %v",
			resp.StatusCode)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return response, err
	}

	err = json.Unmarshal(data, &response)

	return response, err
}
func NewGrafanaClient(url, user, password string, transport *http.Transport, timeoutSeconds time.Duration) GrafanaClient {
	client := &http.Client{
		Transport: transport,
		Timeout:   time.Second * timeoutSeconds,
	}

	return &GrafanaClientImpl{
		url:      url,
		user:     user,
		password: password,
		client:   client,
	}
}

/*
func (i *GrafanaClientImpl) Read(ctx context.Context, client client.Client, ns string) error {
	err := readClusterDataSources(ctx, client, ns)
	if err != nil {
		return err
	}

	var datasourceList []GrafanaDatasourceResponse
	datasourceList, err = i.GetDatasourceList()

	if datasourceList != nil {
		return err
	}

	return nil
}*/
/*
func readClusterDataSources(ctx context.Context, c client.Client, ns string) error {
	list := &v1alpha1.GrafanaDataSourceList{}
	opts := &client.ListOptions{
		Namespace: ns,
	}

	err := c.List(ctx, list, opts)
	i := &GrafanaClientImpl{}
	if err != nil {
		i.ClusterDataSources = list
		return err
	}

	i.ClusterDataSources = list
	return nil
}
*/
