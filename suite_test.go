package eventhub

import (
	"context"
	"errors"
	"flag"
	mgmt "github.com/Azure/azure-sdk-for-go/services/eventhub/mgmt/2017-04-01/eventhub"
	rm "github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2017-05-10/resources"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/suite"
	"math/rand"
	"net/http"
	"os"
	"testing"
	"time"
)

var (
	letterRunes = []rune("abcdefghijklmnopqrstuvwxyz123456789")
	debug       = flag.Bool("debug", false, "output debug level logging")
)

const (
	Location          = "eastus"
	ResourceGroupName = "ehtest"
)

type (
	// eventHubSuite encapsulates a end to end test of Event Hubs with build up and tear down of all EH resources
	eventHubSuite struct {
		suite.Suite
		tenantID       string
		subscriptionID string
		clientID       string
		clientSecret   string
		namespace      string
		env            azure.Environment
		armToken       *adal.ServicePrincipalToken
	}

	servicePrincipalCredentials struct {
		TenantID      string
		ApplicationID string
		Secret        string
	}

	// HubMgmtOption represents an option for configuring an Event Hub.
	hubMgmtOption func(model *mgmt.Model) error
	// NamespaceMgmtOption represents an option for configuring a Namespace
	namespaceMgmtOption func(ns *mgmt.EHNamespace) error
)

func init() {
	rand.Seed(time.Now().Unix())
}

func TestServiceBusSuite(t *testing.T) {
	suite.Run(t, new(eventHubSuite))
}

func (suite *eventHubSuite) SetupSuite() {
	flag.Parse()
	if *debug {
		log.SetLevel(log.DebugLevel)
	}

	suite.tenantID = mustGetEnv("AZURE_TENANT_ID")
	suite.subscriptionID = mustGetEnv("AZURE_SUBSCRIPTION_ID")
	suite.clientID = mustGetEnv("AZURE_CLIENT_ID")
	suite.clientSecret = mustGetEnv("AZURE_CLIENT_SECRET")
	suite.namespace = mustGetEnv("EVENTHUB_NAMESPACE")
	suite.env = azure.PublicCloud
	suite.armToken = suite.servicePrincipalToken()

	err := suite.ensureProvisioned(mgmt.SkuTierStandard)
	if err != nil {
		log.Fatalln(err)
	}
}

func (suite *eventHubSuite) TearDownSuite() {
	// tear down queues and subscriptions maybe??
}

func (suite *eventHubSuite) ensureProvisioned(tier mgmt.SkuTier) error {
	_, err := ensureResourceGroup(context.Background(), suite.subscriptionID, ResourceGroupName, Location, suite.armToken, suite.env)
	if err != nil {
		return err
	}

	_, err = suite.ensureNamespace()
	if err != nil {
		return err
	}

	return nil
}

func (suite *eventHubSuite) servicePrincipalToken() *adal.ServicePrincipalToken {

	oauthConfig, err := adal.NewOAuthConfig(suite.env.ActiveDirectoryEndpoint, suite.tenantID)
	if err != nil {
		log.Fatalln(err)
	}

	tokenProvider, err := adal.NewServicePrincipalToken(*oauthConfig,
		suite.clientID,
		suite.clientSecret,
		suite.env.ResourceManagerEndpoint)
	if err != nil {
		log.Fatalln(err)
	}

	return tokenProvider
}

// ensureResourceGroup creates a Azure Resource Group if it does not already exist
func ensureResourceGroup(ctx context.Context, subscriptionID, name, location string, armToken *adal.ServicePrincipalToken, env azure.Environment) (*rm.Group, error) {
	groupClient := getRmGroupClientWithToken(subscriptionID, armToken, env)
	group, err := groupClient.Get(ctx, name)

	if group.StatusCode == http.StatusNotFound {
		group, err = groupClient.CreateOrUpdate(ctx, name, rm.Group{Location: ptrString(location)})
		if err != nil {
			return nil, err
		}
	} else if group.StatusCode >= 400 {
		return nil, err
	}

	return &group, nil
}

// ensureNamespace creates a Azure Event Hub Namespace if it does not already exist
func ensureNamespace(ctx context.Context, subscriptionID, rg, name, location string, armToken *adal.ServicePrincipalToken, env azure.Environment, opts ...namespaceMgmtOption) (*mgmt.EHNamespace, error) {
	_, err := ensureResourceGroup(ctx, subscriptionID, rg, location, armToken, env)
	if err != nil {
		return nil, err
	}

	client := getNamespaceMgmtClientWithToken(subscriptionID, armToken, env)
	namespace, err := client.Get(ctx, rg, name)
	if err != nil {
		return nil, err
	}

	if namespace.StatusCode == 404 {
		newNamespace := &mgmt.EHNamespace{
			Name: &name,

			Sku: &mgmt.Sku{
				Name:     mgmt.Basic,
				Tier:     mgmt.SkuTierBasic,
				Capacity: ptrInt32(1),
			},
			EHNamespaceProperties: &mgmt.EHNamespaceProperties{
				IsAutoInflateEnabled:   ptrBool(false),
				MaximumThroughputUnits: ptrInt32(1),
			},
		}

		for _, opt := range opts {
			err = opt(newNamespace)
			if err != nil {
				return nil, err
			}
		}

		nsFuture, err := client.CreateOrUpdate(ctx, rg, name, *newNamespace)
		if err != nil {
			return nil, err
		}

		namespace, err = nsFuture.Result(*client)
		if err != nil {
			return nil, err
		}
	} else if namespace.StatusCode >= 400 {
		return nil, err
	}

	return &namespace, nil
}

func (suite *eventHubSuite) ensureEventHub(ctx context.Context, name string, opts ...hubMgmtOption) (*mgmt.Model, error) {
	client := suite.getEventHubMgmtClient()
	hub, err := client.Get(ctx, ResourceGroupName, suite.namespace, name)

	if err != nil {
		newHub := &mgmt.Model{
			Name: &name,
			Properties: &mgmt.Properties{
				PartitionCount: ptrInt64(4),
			},
		}

		for _, opt := range opts {
			err = opt(newHub)
			if err != nil {
				return nil, err
			}
		}

		hub, err = client.CreateOrUpdate(ctx, ResourceGroupName, suite.namespace, name, *newHub)
		if err != nil {
			return nil, err
		}
	}
	return &hub, nil
}

// HubWithPartitions configures an Event Hub to have a specific number of partitions.
//
// Must be between 1 and 32
func hubWithPartitions(count int) hubMgmtOption {
	return func(model *mgmt.Model) error {
		if count < 1 || count > 32 {
			return errors.New("count must be between 1 and 32")
		}
		model.PartitionCount = ptrInt64(int64(count))
		return nil
	}
}

// DeleteEventHub deletes an Event Hub within the given Namespace
func (suite *eventHubSuite) deleteEventHub(ctx context.Context, name string) error {
	client := suite.getEventHubMgmtClient()
	_, err := client.Delete(ctx, ResourceGroupName, suite.namespace, name)
	if err != nil {
		return err
	}
	return nil
}

func (suite *eventHubSuite) getEventHubMgmtClient() *mgmt.EventHubsClient {
	client := mgmt.NewEventHubsClientWithBaseURI(suite.env.ResourceManagerEndpoint, suite.subscriptionID)
	client.Authorizer = autorest.NewBearerAuthorizer(suite.armToken)
	return &client
}

func (suite *eventHubSuite) getNamespaceMgmtClient() *mgmt.NamespacesClient {
	return getNamespaceMgmtClientWithToken(suite.subscriptionID, suite.armToken, suite.env)
}

func getNamespaceMgmtClientWithToken(subscriptionID string, armToken *adal.ServicePrincipalToken, env azure.Environment) *mgmt.NamespacesClient {
	client := mgmt.NewNamespacesClientWithBaseURI(env.ResourceManagerEndpoint, subscriptionID)
	client.Authorizer = autorest.NewBearerAuthorizer(armToken)
	return &client
}

func (suite *eventHubSuite) getNamespaceMgmtClientWithCredentials(ctx context.Context, subscriptionID, rg, name string) *mgmt.NamespacesClient {
	client := mgmt.NewNamespacesClientWithBaseURI(suite.env.ResourceManagerEndpoint, suite.subscriptionID)
	client.Authorizer = autorest.NewBearerAuthorizer(suite.armToken)
	return &client
}

func (suite *eventHubSuite) getRmGroupClient() *rm.GroupsClient {
	return getRmGroupClientWithToken(suite.subscriptionID, suite.armToken, suite.env)
}

func getRmGroupClientWithToken(subscriptionID string, armToken *adal.ServicePrincipalToken, env azure.Environment) *rm.GroupsClient {
	groupsClient := rm.NewGroupsClientWithBaseURI(env.ResourceManagerEndpoint, subscriptionID)
	groupsClient.Authorizer = autorest.NewBearerAuthorizer(armToken)
	return &groupsClient
}

func (suite *eventHubSuite) ensureResourceGroup() (*rm.Group, error) {
	group, err := ensureResourceGroup(context.Background(), suite.subscriptionID, suite.namespace, Location, suite.armToken, suite.env)
	if err != nil {
		return nil, err
	}
	return group, err
}

func (suite *eventHubSuite) ensureNamespace() (*mgmt.EHNamespace, error) {
	ns, err := ensureNamespace(context.Background(), suite.subscriptionID, ResourceGroupName, suite.namespace, Location, suite.armToken, suite.env)
	if err != nil {
		return nil, err
	}
	return ns, err
}

func (suite *eventHubSuite) getEventHubsTokenProvider() (*adal.ServicePrincipalToken, error) {
	// TODO: fix the azure environment var for the SB endpoint and EH endpoint
	return suite.getTokenProvider("https://eventhubs.azure.net/")
}

func (suite *eventHubSuite) getTokenProvider(resourceURI string) (*adal.ServicePrincipalToken, error) {
	oauthConfig, err := adal.NewOAuthConfig(suite.env.ActiveDirectoryEndpoint, suite.tenantID)
	if err != nil {
		log.Fatalln(err)
	}

	tokenProvider, err := adal.NewServicePrincipalToken(*oauthConfig, suite.clientID, suite.clientSecret, resourceURI)
	if err != nil {
		return nil, err
	}

	err = tokenProvider.Refresh()
	if err != nil {
		return nil, err
	}

	return tokenProvider, nil
}

func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic("env variable '" + key + "' required for integration tests.")
	}
	return v
}

func randomName(prefix string, length int) string {
	b := make([]rune, length)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return prefix + "-" + string(b)
}
