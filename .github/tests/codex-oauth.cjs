// Synthetic login and rotating OAuth tokens for the Codex CLI in the kind cluster.
const http = require('node:http')
const claims = {
  sub: 'fixture', email: 'fixture@example.invalid',
  'https://api.openai.com/auth': {chatgpt_plan_type: 'plus', chatgpt_account_id: 'fixture-account', chatgpt_user_id: 'fixture-user'}
}

if (process.argv[2] === 'seed') {
  const id = 'eyJhbGciOiJIUzI1NiJ9.' + Buffer.from(JSON.stringify(claims)).toString('base64url') + '.c2ln'
  console.log(JSON.stringify({
    auth_mode: 'chatgpt', last_refresh: new Date().toISOString(),
    tokens: {access_token: 'fixture', refresh_token: 'fixture-refresh', account_id: 'fixture-account', id_token: id}
  }))
} else {
  let refreshToken = 'fixture-refresh'
  const renewals = [
    {access_token: 'fixture-renewed', refresh_token: 'fixture-rotated'},
    {access_token: 'fixture-day-two-access', refresh_token: 'fixture-day-two-refresh'}
  ]
  http.createServer(async (req, res) => {
    let data = ''
    for await (const chunk of req) data += chunk
    const grant = JSON.parse(data)
    if (req.method !== 'POST' || grant.refresh_token !== refreshToken || renewals.length === 0) {
      res.writeHead(401).end()
      return
    }
    const tokens = renewals.shift()
    refreshToken = tokens.refresh_token
    res.writeHead(200, {'Content-Type': 'application/json'})
    res.end(JSON.stringify(tokens))
  }).listen(18430, '127.0.0.1')
}
