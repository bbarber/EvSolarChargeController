@description('Function App name. Globally unique.')
param functionAppName string

@description('Consumption (Y1) App Service Plan name.')
param appServicePlanName string

@description('Application Insights component name.')
param appInsightsName string

@description('Resource ID of the shared Log Analytics workspace backing Application Insights.')
param logAnalyticsId string

@description('Azure region.')
param location string

param tags object = {}

@description('Storage account holding the Tables and the Functions runtime state.')
param storageAccountName string

@description('Table service endpoint, used for managed-identity data access.')
param tableEndpoint string

@description('Key Vault URI. Secrets are referenced rather than copied into app settings.')
param keyVaultUri string

@description('IANA time zone. The SolarSyncTimer NCRONTAB hours are interpreted in this zone.')
param timeZone string = 'America/Chicago'

@description('Service voltage for the watts -> amps conversion.')
param systemVoltage int = 240

@description('Lowest current the connector will accept.')
param minChargeAmps int = 5

@description('Highest current we will ever request.')
param maxChargeAmps int = 16

@description('First hour of the daylight polling window, local time.')
param pollingStartHourLocal int = 9

@description('Hour the polling window closes, local time, exclusive.')
param pollingEndHourLocal int = 18

@description('Ceiling on Enphase calls per calendar month. The Watt plan allows 1000.')
param enphaseMonthlyCallBudget int = 950

@description('Region-specific Tesla Fleet API base URL.')
param teslaFleetApiBaseUrl string

@description('Direct or Proxy. Vehicles built after 2021 reject unsigned commands, so Proxy is required for them.')
@allowed([
  'Direct'
  'Proxy'
])
param teslaCommandMode string = 'Proxy'

@description('Base URL of the tesla-http-proxy container app. Required when teslaCommandMode is Proxy.')
param teslaCommandProxyBaseUrl string = ''

@description('VINs this controller manages.')
param vins array = []

// Secret names, resolved at runtime through Key Vault references so no secret value ever lands
// in a template, a parameters file, or the deployment history.
var secretNames = {
  enphaseClientId: 'enphase-client-id'
  enphaseClientSecret: 'enphase-client-secret'
  enphaseApiKey: 'enphase-api-key'
  enphaseSystemId: 'enphase-system-id'
  teslaClientId: 'tesla-client-id'
  teslaClientSecret: 'tesla-client-secret'
  ingestSharedSecret: 'ingest-shared-secret'
  proxySharedSecret: 'proxy-shared-secret'
}

func keyVaultRef(vaultUri string, secretName string) string =>
  '@Microsoft.KeyVault(SecretUri=${vaultUri}secrets/${secretName}/)'

var vinSettings = [
  for (vin, i) in vins: {
    name: 'Tesla__Vins__${i}'
    value: vin
  }
]

// Read the account key here rather than accepting a connection string as a parameter, so the key
// never appears in a module output or in deployment history.
resource storageAccount 'Microsoft.Storage/storageAccounts@2023-05-01' existing = {
  name: storageAccountName
}

var storageConnectionString = 'DefaultEndpointsProtocol=https;AccountName=${storageAccount.name};EndpointSuffix=${environment().suffixes.storage};AccountKey=${storageAccount.listKeys().keys[0].value}'

resource appInsights 'Microsoft.Insights/components@2020-02-02' = {
  name: appInsightsName
  location: location
  tags: tags
  kind: 'web'
  properties: {
    Application_Type: 'web'
    WorkspaceResourceId: logAnalyticsId
    IngestionMode: 'LogAnalytics'
    publicNetworkAccessForIngestion: 'Enabled'
    publicNetworkAccessForQuery: 'Enabled'
  }
}

resource appServicePlan 'Microsoft.Web/serverfarms@2023-12-01' = {
  name: appServicePlanName
  location: location
  tags: tags
  sku: {
    name: 'Y1'
    tier: 'Dynamic'
  }
  kind: 'functionapp'
  properties: {
    // Linux, so WEBSITE_TIME_ZONE accepts an IANA id such as America/Chicago.
    reserved: true
  }
}

resource functionApp 'Microsoft.Web/sites@2023-12-01' = {
  name: functionAppName
  location: location
  tags: tags
  kind: 'functionapp,linux'
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    serverFarmId: appServicePlan.id
    httpsOnly: true
    siteConfig: {
      linuxFxVersion: 'DOTNET-ISOLATED|8.0'
      ftpsState: 'Disabled'
      minTlsVersion: '1.2'
      http20Enabled: true
      use32BitWorkerProcess: false
      appSettings: concat(
        [
          {
            name: 'AzureWebJobsStorage'
            value: storageConnectionString
          }
          {
            name: 'FUNCTIONS_EXTENSION_VERSION'
            value: '~4'
          }
          {
            name: 'FUNCTIONS_WORKER_RUNTIME'
            value: 'dotnet-isolated'
          }
          // No WEBSITE_RUN_FROM_PACKAGE here on purpose. On the Linux Consumption plan it must be a
          // blob URL rather than '1', and setting it to '1' makes zip deployment fail with an
          // opaque "Bad Request". The deployment tooling sets the correct value itself.
          {
            name: 'WEBSITE_TIME_ZONE'
            value: timeZone
          }
          {
            name: 'APPLICATIONINSIGHTS_CONNECTION_STRING'
            value: appInsights.properties.ConnectionString
          }
          {
            name: 'Storage__ServiceUri'
            value: tableEndpoint
          }
          {
            name: 'SecretStore__KeyVaultUri'
            value: keyVaultUri
          }
          {
            name: 'Charging__SystemVoltage'
            value: string(systemVoltage)
          }
          {
            name: 'Charging__MinChargeAmps'
            value: string(minChargeAmps)
          }
          {
            name: 'Charging__MaxChargeAmps'
            value: string(maxChargeAmps)
          }
          {
            name: 'PollingWindow__TimeZone'
            value: timeZone
          }
          {
            name: 'PollingWindow__StartHourLocal'
            value: string(pollingStartHourLocal)
          }
          {
            name: 'PollingWindow__EndHourLocal'
            value: string(pollingEndHourLocal)
          }
          {
            name: 'Enphase__MonthlyCallBudget'
            value: string(enphaseMonthlyCallBudget)
          }
          {
            name: 'Enphase__ClientId'
            value: keyVaultRef(keyVaultUri, secretNames.enphaseClientId)
          }
          {
            name: 'Enphase__ClientSecret'
            value: keyVaultRef(keyVaultUri, secretNames.enphaseClientSecret)
          }
          {
            name: 'Enphase__ApiKey'
            value: keyVaultRef(keyVaultUri, secretNames.enphaseApiKey)
          }
          {
            name: 'Enphase__SystemId'
            value: keyVaultRef(keyVaultUri, secretNames.enphaseSystemId)
          }
          {
            name: 'Tesla__ClientId'
            value: keyVaultRef(keyVaultUri, secretNames.teslaClientId)
          }
          {
            name: 'Tesla__ClientSecret'
            value: keyVaultRef(keyVaultUri, secretNames.teslaClientSecret)
          }
          {
            name: 'Tesla__FleetApiBaseUrl'
            value: teslaFleetApiBaseUrl
          }
          {
            name: 'Tesla__CommandMode'
            value: teslaCommandMode
          }
          {
            name: 'Tesla__CommandProxyBaseUrl'
            value: teslaCommandProxyBaseUrl
          }
          {
            name: 'Tesla__CommandProxySharedSecret'
            value: keyVaultRef(keyVaultUri, secretNames.proxySharedSecret)
          }
          {
            name: 'Ingest__SharedSecret'
            value: keyVaultRef(keyVaultUri, secretNames.ingestSharedSecret)
          }
        ],
        vinSettings
      )
    }
  }
}

output functionAppName string = functionApp.name
output functionAppId string = functionApp.id
output defaultHostName string = functionApp.properties.defaultHostName
output principalId string = functionApp.identity.principalId
