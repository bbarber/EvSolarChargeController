// Solar-to-Tesla amp matcher — infrastructure.
//
// Deploy with:
//   az deployment group create -g <rg> -f infra/main.bicep -p infra/main.bicepparam
//
// Secret *values* are deliberately absent from this template and from the parameters file. Seed
// them into Key Vault out-of-band (tools/seed-keyvault.sh) and the Function App picks them up
// through Key Vault references.

targetScope = 'resourceGroup'

@description('Short environment name, used in resource names. Lowercase alphanumerics.')
@minLength(2)
@maxLength(8)
param environmentName string = 'prod'

@description('Azure region for all resources.')
param location string = resourceGroup().location

@description('Prefix for resource names. Lowercase alphanumerics, kept short because storage account names cap at 24 characters.')
@minLength(3)
@maxLength(11)
param namePrefix string = 'evsolar'

@description('IANA time zone driving the daylight polling window.')
param timeZone string = 'America/Chicago'

@description('Service voltage for the watts -> amps conversion. 240 is US residential split-phase.')
param systemVoltage int = 240

@description('Lowest current the wall connector will accept.')
param minChargeAmps int = 5

@description('Highest current to request. Capped at the array peak so we never pull the difference from the grid.')
param maxChargeAmps int = 16

@description('First hour of the polling window, local time.')
param pollingStartHourLocal int = 9

@description('Hour the polling window closes, local time, exclusive.')
param pollingEndHourLocal int = 19

@description('Monthly Enphase call ceiling. The free Watt plan allows 1000.')
param enphaseMonthlyCallBudget int = 950

@description('Region-specific Tesla Fleet API base URL.')
param teslaFleetApiBaseUrl string = 'https://fleet-api.prd.na.vn.cloud.tesla.com'

@description('Direct or Proxy. Vehicles built after 2021 reject unsigned commands and need Proxy.')
@allowed([
  'Direct'
  'Proxy'
])
param teslaCommandMode string = 'Proxy'

@description('VINs this controller manages.')
param vins array = []

@description('Deploy the Container Apps that host fleet-telemetry and the Tesla command proxy. Requires the container images to exist.')
param deployContainerApps bool = false

@description('Container image for the ZMQ -> HTTP telemetry bridge.')
param bridgeImage string = ''

@description('Container image for the Tesla vehicle-command HTTP proxy.')
param teslaProxyImage string = ''

@description('Deploy API Management to host the Tesla third-party public key on a custom domain.')
param deployApim bool = false

@description('Publisher email for API Management.')
param apimPublisherEmail string = ''

@description('Publisher organisation for API Management.')
param apimPublisherName string = 'EvSolarChargeController'

@description('Custom domain for the APIM gateway, e.g. tesla.example.com.')
param apimCustomDomainName string = ''

@description('Key Vault secret ID of the TLS certificate for the APIM custom domain.')
@secure()
param apimCustomDomainCertificateSecretId string = ''

@description('PEM-encoded EC public key Tesla fetches to verify the application.')
param teslaPublicKeyPem string = ''

@description('Shared secret the telemetry bridge presents to the ingest Function. Generate a fresh random value; never commit it.')
@secure()
param ingestSharedSecret string = ''

@description('Shared secret required on inbound calls to the Tesla command proxy.')
@secure()
param proxySharedSecret string = ''

var tags = {
  application: 'EvSolarChargeController'
  environment: environmentName
  managedBy: 'bicep'
}

var uniquePart = uniqueString(resourceGroup().id)
var resourceBaseName = '${namePrefix}-${environmentName}'
var storageAccountName = toLower(take('st${namePrefix}${environmentName}${uniquePart}', 24))
var keyVaultName = take('kv-${namePrefix}-${environmentName}-${uniquePart}', 24)
var functionAppName = 'func-${resourceBaseName}-${uniquePart}'
var apimServiceName = 'apim-${resourceBaseName}-${uniquePart}'

module logAnalytics 'modules/loganalytics.bicep' = {
  name: 'loganalytics'
  params: {
    logAnalyticsName: 'log-${resourceBaseName}'
    location: location
    tags: tags
  }
}

module storage 'modules/storage.bicep' = {
  name: 'storage'
  params: {
    storageAccountName: storageAccountName
    location: location
    tags: tags
  }
}

// Deployed before the Function App because the app's Tesla__CommandProxyBaseUrl setting points at
// the proxy. The bridge reaches the Function by its well-known hostname, so nothing here needs an
// output from the Function App — which is what keeps the two modules acyclic.
module containerApps 'modules/containerapps.bicep' = if (deployContainerApps) {
  name: 'containerapps'
  params: {
    namePrefix: resourceBaseName
    location: location
    tags: tags
    logAnalyticsName: logAnalytics.outputs.logAnalyticsName
    storageAccountName: storage.outputs.storageAccountName
    bridgeImage: bridgeImage
    teslaProxyImage: teslaProxyImage
    ingestUrl: 'https://${functionAppName}.azurewebsites.net/api/telemetry'
    ingestSharedSecret: ingestSharedSecret
    proxySharedSecret: proxySharedSecret
  }
}

module keyVault 'modules/keyvault.bicep' = {
  name: 'keyvault'
  params: {
    keyVaultName: keyVaultName
    location: location
    tags: tags
  }
}

module functionApp 'modules/functionapp.bicep' = {
  name: 'functionapp'
  params: {
    functionAppName: functionAppName
    appServicePlanName: 'plan-${resourceBaseName}'
    appInsightsName: 'appi-${resourceBaseName}'
    logAnalyticsId: logAnalytics.outputs.logAnalyticsId
    location: location
    tags: tags
    storageAccountName: storage.outputs.storageAccountName
    tableEndpoint: storage.outputs.tableEndpoint
    keyVaultUri: keyVault.outputs.keyVaultUri
    timeZone: timeZone
    systemVoltage: systemVoltage
    minChargeAmps: minChargeAmps
    maxChargeAmps: maxChargeAmps
    pollingStartHourLocal: pollingStartHourLocal
    pollingEndHourLocal: pollingEndHourLocal
    enphaseMonthlyCallBudget: enphaseMonthlyCallBudget
    teslaFleetApiBaseUrl: teslaFleetApiBaseUrl
    teslaCommandMode: teslaCommandMode
    teslaCommandProxyBaseUrl: containerApps.?outputs.proxyBaseUrl ?? ''
    vins: vins
  }
}

// Granted after the Function App exists, because the assignment needs its managed identity.
module rbac 'modules/rbac.bicep' = {
  name: 'rbac'
  params: {
    keyVaultName: keyVault.outputs.keyVaultName
    storageAccountName: storage.outputs.storageAccountName
    principalIds: [
      functionApp.outputs.principalId
    ]
  }
}

module apim 'modules/apim.bicep' = if (deployApim) {
  name: 'apim'
  params: {
    apimName: apimServiceName
    location: location
    tags: tags
    publisherEmail: apimPublisherEmail
    publisherName: apimPublisherName
    customDomainName: apimCustomDomainName
    customDomainCertificateSecretId: apimCustomDomainCertificateSecretId
    teslaPublicKeyPem: teslaPublicKeyPem
  }
}

output functionAppName string = functionApp.outputs.functionAppName
output functionAppHostName string = functionApp.outputs.defaultHostName
output telemetryIngestUrl string = 'https://${functionApp.outputs.defaultHostName}/api/telemetry'
output keyVaultName string = keyVault.outputs.keyVaultName
output storageAccountName string = storage.outputs.storageAccountName
output telemetryFqdn string = containerApps.?outputs.telemetryFqdn ?? ''
output teslaCommandProxyUrl string = containerApps.?outputs.proxyBaseUrl ?? ''
output teslaPublicKeyUrl string = apim.?outputs.publicKeyUrl ?? ''

@description('Reminder of the manual steps Bicep cannot express.')
output nextSteps array = [
  'Seed secrets: ./tools/seed-keyvault.sh ${keyVault.outputs.keyVaultName}'
  'Deploy function code: func azure functionapp publish ${functionApp.outputs.functionAppName}'
  'Complete the Tesla and Enphase OAuth flows, then re-run the seed script'
  'Register fleet_telemetry_config on each vehicle once the telemetry endpoint is live'
]
