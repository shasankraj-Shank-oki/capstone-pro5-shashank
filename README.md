# Order Management System (Go Lang)

Gateway + private services, all run as plain Go processes. HTTP only (no TLS).

```text
Client -> API Gateway :8080 -> Order Service :8081 -> PostgreSQL (orders)
                                     |               |
                                     |               +-> Product Service :8082 -> PostgreSQL (products, reservations)
                                     +-> Product Service :8082 (product API)
```

| Service | Folder | Responsibility |
| --- | --- | --- |
| Gateway | `cmd/gateway`, `internal/gateway` | Public entry point. Login, JWT + role checks, security headers, CORS, request IDs, logs, metrics, forwards order and product APIs |
| Order Service | `cmd/order-service`, `internal/orders` | Order operations (REST + GraphQL), stock reservation/release calls, validation, GORM repository |
| Product Service | `cmd/product-service`, `internal/product` | GORM-backed products and stock, admin catalog/stock APIs, internal price and reservation API |

## Project structure

The case study's sample tree is for one executable. This project keeps the already-working services as separate Go commands and groups the order API by layer:

```text
cmd/
  gateway/          Public API gateway
  order-service/    Order API process
  product-service/ Product catalog and inventory process
internal/
  auth/             JWT authentication and roles
  config/           Environment configuration
  database/         GORM connection, pool settings, schema, query logging
  gateway/          Routing, middleware, and forwarding
  observability/    Request IDs, logs, and metrics
  orders/
    handler.go      REST and GraphQL controllers
    model.go        Order model, inputs, and interfaces
    service.go      Order business logic and validation
    repository.go   GORM repository (PostgreSQL)
  product/          Product Service API, GORM repository, and client resilience
buildspec.yml       AWS CodeBuild: format, vet, tests, security scans, build
deploy/beanstalk/   Elastic Beanstalk bundle files (Procfile, .ebextensions)
deploy/aws/         CloudWatch agent config (and legacy EC2/systemd files, unused by Beanstalk)
```

Request flow: the order service reads a product price, persists the order, then reserves stock through the Product Service. It marks the order `CONFIRMED` only after the reservation succeeds. If reservation or confirmation fails, it compensates by releasing any possibly-created reservation and removes the failed order (or marks it `CANCELLED` if cleanup cannot complete). Cancelling an order releases its reservation; release operations are idempotent, so an admin can retry cancellation if the Product Service is temporarily unavailable.
Request ID (`X-Request-ID`) is created at the gateway and forwarded to the Order Service and the Product Service.

## Prerequisites
- Go 1.26+
- PostgreSQL 14+ running locally, and `psql` available

## Setup

1. Create the database (the services create the `orders`, `products`, and `stock_reservations` tables on startup):
   ```
   psql -U postgres -c "CREATE DATABASE oms;"
   ```
2. Create `.env`:
   - Linux/macOS: `cp .env.example .env`
   - PowerShell: `Copy-Item .env.example .env`
3. Edit `.env`:
   - `DB_USER`, `DB_PASSWORD`: your PostgreSQL login
   - `JWT_SECRET`: at least 32 characters (`openssl rand -base64 48`, or any long random string)
4. Set at least one login password (see Authentication): `AUTH_ADMIN_PASSWORD` and/or `AUTH_USER_PASSWORD` (12+ characters).

Database access uses GORM (`gorm.io/gorm` + `gorm.io/driver/postgres`). Both services apply the shared idempotent schema from `internal/database/schema.sql` while holding a PostgreSQL advisory lock, so simultaneous startup cannot race table/type creation. Pool size comes from `DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`, `DB_CONN_MAX_LIFETIME`; TLS to PostgreSQL from `DB_SSLMODE` (use `require` for RDS).

Never commit `.env` (it is git-ignored).

## Run (3 terminals, from the project root)

```
go run ./cmd/product-service      # terminal 1  :8082
go run ./cmd/order-service        # terminal 2  :8081
go run ./cmd/gateway              # terminal 3  :8080
```
Start them in this order. Each prints JSON log lines. Stop with Ctrl+C (graceful shutdown).

## Verify everything automatically

```
go test ./...       # unit tests (no database needed)
go test -tags integration ./internal/orders   # optional, needs PostgreSQL
```

Gateway tests cover authentication, order creation, product inventory reservations, cancellation, and request forwarding using in-memory dependencies.

## Manual checks by topic

| # | Topic | How to check |
| --- | --- | --- |
| 1 | Go REST API and validation | `go test ./internal/orders ./internal/gateway` |
| 3 | PostgreSQL | `psql -U <DB_USER> -d oms -c "SELECT id,name,price,stock FROM products;"` and `psql -U <DB_USER> -d oms -c "SELECT id,customer_id,product_id,quantity,unit_price,total_amount,status FROM orders;"` |
| 4 | Environment config | Set `APP_PORT=9090` and restart the gateway. Missing `JWT_SECRET` or database credentials produces a startup error. |
| 8 | Input validation | Order handler tests cover malformed requests, invalid IDs, pagination, and status values. |
| 10 | Authentication / authorization | Order and gateway tests cover customer and admin access. |
| 11 | Structured logging | Service logs include `request_id`, `method`, `path`, `status`, and `duration`. |
| 12 | Metrics | `curl localhost:8080/metrics` (gateway) and `curl localhost:8081/metrics` (order service, includes `database_errors_total`). Look for `*_http_requests_total`, `*_http_request_duration_seconds`, `*_http_errors_total` |
| 13 | Health checks | `curl localhost:8080/health/live` and `/health/ready` -> 200. Stop PostgreSQL: `localhost:8081/health/ready` and `localhost:8080/health/ready` return 503 while `/health/live` stays 200. Start PostgreSQL again: ready returns 200 |
| 15 | Product stock and order flow | Admin creates a product with initial stock, customer creates an order, stock decreases by the ordered quantity, and admin cancellation restores it. |
| 16 | Insufficient stock | Create an order whose quantity exceeds available stock; the API responds `409 insufficient_stock` and compensates the failed order. |
| 17 | Idempotent cancellation | Repeat cancellation after a successful cancellation; stock is restored only once. If Product Service is down, cancellation returns `503` and can be retried after it recovers. |
| 18 | Product client resilience | Price lookup timeout, bounded retries, and circuit breaker tests: `go test ./internal/product -v` |

## Authentication

There is no token CLI. Log in through the gateway to get a JWT:

```
curl -s -X POST localhost:8080/auth/login -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"<AUTH_ADMIN_PASSWORD>"}'
```
Response: `{"token":"<jwt>","tokenType":"Bearer","expiresIn":900}`. Send it as `Authorization: Bearer <jwt>`.

| Account | Username env (default) | Password env | Role | Can do |
| --- | --- | --- | --- | --- |
| Admin | `AUTH_ADMIN_USERNAME` (`admin`) | `AUTH_ADMIN_PASSWORD` | `admin` | create/delete products and update/cancel orders |
| User | `AUTH_USER_USERNAME` (`user`) | `AUTH_USER_PASSWORD` | `customer` | browse products and create/get/list orders |

Passwords come from the environment (Secrets Manager in AWS) and must be at least 12 characters. A role with no password set has no account, so login is off until you configure one. Tokens are HS256, signed with `JWT_SECRET`, and expire after `JWT_TTL` (default 15m). This login is meant for testing and small deployments; use an identity provider such as Amazon Cognito for real end users. There is no login rate limit yet; add one (for example AWS WAF on the load balancer) before exposing it publicly.

PowerShell tip: use `curl.exe` (plain `curl` is an alias for something else).

## REST API (all via gateway, JWT required)

| Method | Path | Role |
| --- | --- | --- |
| POST | `/api/v1/orders` body `{"customerId":101,"productId":501,"quantity":2}` | customer, admin |
| GET | `/api/v1/orders/{id}` | customer, admin |
| GET | `/api/v1/orders?limit=50&offset=0` (limit 1-100) | customer, admin |
| PUT | `/api/v1/orders/{id}/status` body `{"status":"CONFIRMED"}` (`CREATED`, `CONFIRMED`, `CANCELLED`) | admin |
| GET | `/api/v1/products?limit=50&offset=0` | customer, admin |
| GET | `/api/v1/products/{id}` | customer, admin |
| POST | `/api/v1/products` body `{"name":"Keyboard","price":49.99,"stock":10}` | admin |
| DELETE | `/api/v1/products/{id}` | admin (rejected while the product has active order reservations) |

Errors: `{"error":{"code":"...","message":"...","requestId":"..."}}`. Internal details (SQL, stack, credentials) are never returned.

## GraphQL (`POST /graphql`, same JWT)

Schema and example operations: `internal/orders/schema.graphqls`. Queries `order(id)`, `orders(limit, offset)`; mutations `createOrder(input)`, `updateOrderStatus(id, status)` (admin only).

```
curl -X POST localhost:8080/graphql -H "Authorization: Bearer <token>" -H "Content-Type: application/json" \
  -d '{"query":"{ orders(limit:5){ id status totalAmount createdAt } }"}'
```

## Security notes
- Gateway adds `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`, `Cache-Control: no-store`, `Content-Security-Policy`.
- CORS is off by default. Set `CORS_ALLOWED_ORIGINS=http://localhost:3000` (comma-separated) to allow specific origins.
- All database access goes through GORM with bound parameters. Secrets come only from the environment / Secrets Manager (`.env` is git-ignored).
- TLS is intentionally not configured. For production, put an HTTPS reverse proxy in front of the gateway.

## AWS deployment (no Docker): CodeBuild + CodePipeline + Elastic Beanstalk + RDS

```text
Source repo -> CodePipeline -> CodeBuild (buildspec.yml) -> Elastic Beanstalk (Go platform, no containers)
                                                              |  Load balancer -> gateway :5000 -> order-service :8081 -> RDS PostgreSQL
                                                              |                  |                +-> product-service :8082 -> RDS PostgreSQL
                                                              |                  +-> product-service :8082
```
Elastic Beanstalk runs the three Go binaries as separate processes on each instance (`deploy/beanstalk/Procfile`). Only the gateway listens on the port the load balancer reaches (5000); ports 8081 and 8082 stay on the instance. Note: **RDS is the PostgreSQL database; S3 is not the database.** CodePipeline uses an S3 bucket only to hand build artifacts between stages.

### 1. Network and database
1. Create a VPC with public subnets (load balancer) and private subnets (instances, RDS).
2. Create an RDS PostgreSQL instance in the private subnets. Create the database named by `DB_NAME` (the local example uses `oms`). Security group: allow 5432 only from the Beanstalk instance security group.
3. The services create the orders, products, and stock reservation tables on startup, so the DB user needs CREATE/ALTER rights on the schema.

### 2. Secrets
Create a Secrets Manager secret (or one per value) for `DB_USER`, `DB_PASSWORD`, `JWT_SECRET`, `AUTH_ADMIN_PASSWORD`, `AUTH_USER_PASSWORD`. Never put them in the repo, `buildspec.yml`, or `.ebextensions`. In the Beanstalk environment, add them as environment properties that reference Secrets Manager (Configuration > Updates, monitoring, and logging > Environment properties; check the current AWS documentation for the exact secret-reference option), and set `DB_HOST` to the RDS endpoint and `DB_NAME`. Give the Beanstalk instance profile `secretsmanager:GetSecretValue` on just those secrets. Non-secret defaults are in `deploy/beanstalk/.ebextensions/01-environment.config` (`APP_PORT=5000`, `DB_SSLMODE=require`, internal service URLs).

### 3. Elastic Beanstalk
1. Create an application and a load-balanced environment on a current **Go** platform (AL2023), in the VPC above. Instances in private subnets, load balancer in public subnets.
2. The instance profile needs `AWSElasticBeanstalkWebTier` plus the Secrets Manager read above and CloudWatch Logs permissions.
3. Health check path is set to `/health/ready` by the bundle. Enable CloudWatch log streaming in the environment's log settings; for metrics see `deploy/aws/cloudwatch-agent.json` and `prometheus.yml` (scrapes `/metrics` on 5000 and 8081).
4. The bundle builds for `linux/amd64`. If you choose Graviton (arm64) instances, change `GOARCH` in `buildspec.yml`.

### 4. CodeBuild and CodePipeline
1. Create a CodeBuild project: source = your repo, environment = Amazon Linux standard image, buildspec = `buildspec.yml`. The build needs outbound internet (or VPC endpoints plus a Go module proxy) to download modules and the scanners.
2. The build runs, in order: `gofmt` check, `go vet`, `go test ./...`, `govulncheck`, `gosec -severity high`, then compiles the three binaries into the Beanstalk bundle. Any failure stops the build, so the pipeline never deploys it.
3. Create a CodePipeline with three stages: Source (CodeConnections to GitHub/GitLab/Bitbucket, or CodeCommit), Build (the CodeBuild project), Deploy (action provider **AWS Elastic Beanstalk**, your application and environment).
4. IAM: the CodeBuild role needs CloudWatch Logs and read/write on the pipeline artifact bucket. The CodePipeline role needs access to the artifact bucket, CodeBuild, and Elastic Beanstalk. No AWS credentials are stored anywhere in the repo.

### 5. After the first deploy
`curl https://<env-url>/health/ready` should return `{"status":"READY"}`; then log in via `/auth/login`, create a product as admin, and create an order with available stock.

### HTTPS
TLS is not implemented in the application. In production terminate HTTPS at the load balancer with an ACM certificate and redirect HTTP to HTTPS.

## Troubleshooting
- `connect to database` / `ping PostgreSQL` error: PostgreSQL not running, database named by `DB_NAME` missing, or wrong `DB_USER`/`DB_PASSWORD`/`DB_HOST`.
- `permission denied` creating table: the DB user needs rights on the database/schema.
- `missing go.sum entry` / cannot find `gorm.io/...`: run `go get gorm.io/gorm gorm.io/driver/postgres && go mod tidy` and commit `go.mod`/`go.sum`.
- `no pg_hba.conf entry ... no encryption` on RDS: set `DB_SSLMODE=require`.
- `address already in use`: change `APP_PORT`, `ORDER_SERVICE_PORT` or `PRODUCT_SERVICE_PORT` in `.env`.
- 503 on create: product service not running or too slow (see timeout/breaker above). Wait `PRODUCT_BREAKER_RECOVERY` after starting it.
- 401 with a fresh token: `JWT_SECRET` differs between services, or the token expired.
- Commands must be run from the project root (that is where `.env` is read).

## Known limitations
- No distributed tracing exporter (no OpenTelemetry/X-Ray). Correlation is by `X-Request-ID`; each GORM query is logged with that ID and its duration (debug level on success, error level on failure).
- Customers can read any order (no ownership rule; not in the requirements).
- The PostgreSQL integration test is opt-in: `go test -tags integration ./internal/orders` with `DB_USER`/`DB_PASSWORD` (and `DB_HOST`, `DB_NAME`) set. Other tests use in-memory fakes.
- Orders, products, and stock reservation tables are created at startup from `internal/database/schema.sql`.
