@description('Storage account name. Must be globally unique, 3-24 lowercase alphanumerics.')
param storageAccountName string

@description('Azure region.')
param location string

@description('Tags applied to every resource.')
param tags object = {}

@description('Tables created up front so the Functions never race to create them.')
param tableNames array = [
  'VehicleState'
  'SolarReadings'
  'ApiUsage'
  'Secrets'
]

resource storageAccount 'Microsoft.Storage/storageAccounts@2023-05-01' = {
  name: storageAccountName
  location: location
  tags: tags
  sku: {
    name: 'Standard_LRS'
  }
  kind: 'StorageV2'
  properties: {
    minimumTlsVersion: 'TLS1_2'
    supportsHttpsTrafficOnly: true
    allowBlobPublicAccess: false
    // The Functions host on the Consumption plan still needs key-based access for
    // AzureWebJobsStorage; application data access goes through managed identity.
    allowSharedKeyAccess: true
    accessTier: 'Hot'
    networkAcls: {
      bypass: 'AzureServices'
      defaultAction: 'Allow'
    }
  }
}

resource tableService 'Microsoft.Storage/storageAccounts/tableServices@2023-05-01' = {
  parent: storageAccount
  name: 'default'
}

resource tables 'Microsoft.Storage/storageAccounts/tableServices/tables@2023-05-01' = [
  for tableName in tableNames: {
    parent: tableService
    name: tableName
  }
]

output storageAccountId string = storageAccount.id
output storageAccountName string = storageAccount.name
output tableEndpoint string = storageAccount.properties.primaryEndpoints.table

// No connection-string output on purpose: an output carrying an account key lands in the
// deployment history in plain text. Consumers re-read the key via an `existing` reference.
