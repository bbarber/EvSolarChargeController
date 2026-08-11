@description('Key Vault name. Globally unique, 3-24 characters.')
param keyVaultName string

@description('Azure region.')
param location string

param tags object = {}

@description('Soft-delete retention. 7 is the minimum and keeps a rotated token recoverable.')
param softDeleteRetentionInDays int = 7

// Role assignments live in modules/rbac.bicep instead of here. The Function App needs the vault
// URI in its app settings while the vault needs the app's principal id for its role assignment,
// so splitting them breaks what would otherwise be a circular dependency.
resource keyVault 'Microsoft.KeyVault/vaults@2023-07-01' = {
  name: keyVaultName
  location: location
  tags: tags
  properties: {
    sku: {
      family: 'A'
      name: 'standard'
    }
    tenantId: subscription().tenantId
    // RBAC rather than access policies: assignments stay visible in Bicep and survive identity
    // recreation more predictably.
    enableRbacAuthorization: true
    enableSoftDelete: true
    softDeleteRetentionInDays: softDeleteRetentionInDays
    publicNetworkAccess: 'Enabled'
    networkAcls: {
      bypass: 'AzureServices'
      defaultAction: 'Allow'
    }
  }
}

output keyVaultId string = keyVault.id
output keyVaultName string = keyVault.name
output keyVaultUri string = keyVault.properties.vaultUri
