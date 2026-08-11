@description('Name of the Key Vault to grant access on.')
param keyVaultName string

@description('Name of the Storage Account to grant access on.')
param storageAccountName string

@description('Managed identity principal IDs that read and write secrets and table data.')
param principalIds array

// Built-in role definition IDs.
// Secrets Officer (not just User) because both Enphase and Tesla hand back a new refresh token on
// every refresh, and the old one stops working — the app has to write the replacement back.
var keyVaultSecretsOfficerRoleId = 'b86a8fe4-44ce-4948-aee5-eccb2c155cd7'
var storageTableDataContributorRoleId = '0a9a7e1f-b9d0-4cc4-a60d-0319b160aaa3'

resource keyVault 'Microsoft.KeyVault/vaults@2023-07-01' existing = {
  name: keyVaultName
}

resource storageAccount 'Microsoft.Storage/storageAccounts@2023-05-01' existing = {
  name: storageAccountName
}

resource secretsAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = [
  for principalId in principalIds: {
    name: guid(keyVault.id, principalId, keyVaultSecretsOfficerRoleId)
    scope: keyVault
    properties: {
      roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', keyVaultSecretsOfficerRoleId)
      principalId: principalId
      principalType: 'ServicePrincipal'
    }
  }
]

resource tableAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = [
  for principalId in principalIds: {
    name: guid(storageAccount.id, principalId, storageTableDataContributorRoleId)
    scope: storageAccount
    properties: {
      roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', storageTableDataContributorRoleId)
      principalId: principalId
      principalType: 'ServicePrincipal'
    }
  }
]
