// Shared Log Analytics workspace. Owned by its own module because both the Function App (via
// Application Insights) and the Container Apps environment need it, and nesting it inside either
// one would create a dependency cycle between them.

@description('Workspace name.')
param logAnalyticsName string

@description('Azure region.')
param location string

param tags object = {}

@description('Daily ingestion cap. This workload writes a trickle of logs; the cap stops a runaway loop becoming a bill.')
param dailyQuotaGb int = 1

resource logAnalytics 'Microsoft.OperationalInsights/workspaces@2023-09-01' = {
  name: logAnalyticsName
  location: location
  tags: tags
  properties: {
    sku: {
      name: 'PerGB2018'
    }
    retentionInDays: 30
    workspaceCapping: {
      dailyQuotaGb: dailyQuotaGb
    }
  }
}

output logAnalyticsId string = logAnalytics.id
output logAnalyticsName string = logAnalytics.name
output customerId string = logAnalytics.properties.customerId
