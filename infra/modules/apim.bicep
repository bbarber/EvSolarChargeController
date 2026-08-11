// API Management, Consumption tier.
//
// Scope note: the original design put APIM in front of telemetry ingest to perform mTLS. That is
// not workable — Tesla vehicles speak a mutual-TLS WebSocket and the receiving server must read
// the client certificate off the TLS state itself, so the handshake cannot be terminated at a
// gateway. Telemetry ingest therefore runs on Container Apps (see containerapps.bicep).
//
// What remains genuinely useful here is hosting the Tesla third-party public key on a custom
// domain, which is a prerequisite for pairing a virtual key to a vehicle, and which APIM
// Consumption serves inside its free monthly call allotment.

@description('APIM service name. Globally unique.')
param apimName string

@description('Azure region.')
param location string

param tags object = {}

@description('Publisher email shown on the developer portal and used for service notifications.')
param publisherEmail string

@description('Publisher organisation name.')
param publisherName string

@description('Custom domain for the gateway, e.g. tesla.example.com. Leave empty to use the default *.azure-api.net hostname.')
param customDomainName string = ''

@description('Key Vault secret URI of the TLS certificate for the custom domain. Required when customDomainName is set.')
@secure()
param customDomainCertificateSecretId string = ''

@description('Managed identity resource ID that can read the certificate from Key Vault.')
param certificateIdentityResourceId string = ''

@description('PEM-encoded EC public key that Tesla fetches to verify the application.')
param teslaPublicKeyPem string

var useCustomDomain = !empty(customDomainName)

resource apim 'Microsoft.ApiManagement/service@2023-05-01-preview' = {
  name: apimName
  location: location
  tags: tags
  sku: {
    name: 'Consumption'
    capacity: 0
  }
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    publisherEmail: publisherEmail
    publisherName: publisherName
    hostnameConfigurations: useCustomDomain
      ? [
          {
            type: 'Proxy'
            hostName: customDomainName
            keyVaultId: customDomainCertificateSecretId
            identityClientId: empty(certificateIdentityResourceId) ? null : certificateIdentityResourceId
            negotiateClientCertificate: false
            defaultSslBinding: true
          }
        ]
      : []
  }
}

// The key is public by design, so a named value (not a secret) is the right home for it.
resource publicKeyNamedValue 'Microsoft.ApiManagement/service/namedValues@2023-05-01-preview' = {
  parent: apim
  name: 'tesla-public-key-pem'
  properties: {
    displayName: 'tesla-public-key-pem'
    value: teslaPublicKeyPem
    secret: false
  }
}

resource publicKeyApi 'Microsoft.ApiManagement/service/apis@2023-05-01-preview' = {
  parent: apim
  name: 'tesla-public-key'
  properties: {
    displayName: 'Tesla third-party public key'
    path: '.well-known/appspecific'
    protocols: [
      'https'
    ]
    subscriptionRequired: false
    apiRevision: '1'
  }
}

resource publicKeyOperation 'Microsoft.ApiManagement/service/apis/operations@2023-05-01-preview' = {
  parent: publicKeyApi
  name: 'get-public-key'
  properties: {
    displayName: 'Get com.tesla.3p.public-key.pem'
    method: 'GET'
    urlTemplate: '/com.tesla.3p.public-key.pem'
    responses: [
      {
        statusCode: 200
        description: 'PEM-encoded EC public key.'
      }
    ]
  }
}

resource publicKeyPolicy 'Microsoft.ApiManagement/service/apis/operations/policies@2023-05-01-preview' = {
  parent: publicKeyOperation
  name: 'policy'
  properties: {
    format: 'rawxml'
    value: loadTextContent('../policies/public-key.policy.xml')
  }
  dependsOn: [
    publicKeyNamedValue
  ]
}

output apimName string = apim.name
output gatewayUrl string = apim.properties.gatewayUrl
output publicKeyUrl string = useCustomDomain
  ? 'https://${customDomainName}/.well-known/appspecific/com.tesla.3p.public-key.pem'
  : '${apim.properties.gatewayUrl}/.well-known/appspecific/com.tesla.3p.public-key.pem'
