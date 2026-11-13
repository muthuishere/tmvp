# System references

One file per tmvp type: how to find it in code, what to ask, config, a read-only proof, and what to write into the product skill.

| type | file | typical code evidence |
|---|---|---|
| aws | [aws.md](aws.md) | aws-sdk/boto3, S3/SES env names, terraform aws |
| az | [az.md](az.md) | @azure/*, azure-pipelines, bicep |
| elasticsearch | [elasticsearch.md](elasticsearch.md) | elasticsearch clients/images |
| gcp | [gcp.md](gcp.md) | @google-cloud/*, cloudbuild/app.yaml |
| gh | [gh.md](gh.md) | `git remote` on github.com, .github/workflows |
| git | [git.md](git.md) | non-GitHub remotes, deploy tags |
| grafana | [grafana.md](grafana.md) | grafana images, provisioned dashboards |
| grpc | [grpc.md](grpc.md) | `.proto`/buf files, `grpc.NewServer`/`reflection.Register`, `*_GRPC_ADDR` names, port 50051 |
| http | [http.md](http.md) | SaaS SDKs (payments, email, LLMs), base URLs, OpenAPI |
| k8s | [k8s.md](k8s.md) | manifests, helm charts, kubectl/helm in CI |
| kafka | [kafka.md](kafka.md) | kafkajs/sarama/spring-kafka, topics |
| loki | [loki.md](loki.md) | loki/promtail/alloy, OTEL exporters |
| mongo | [mongo.md](mongo.md) | mongoose/pymongo/mongo-driver, MONGODB_URI |
| mysql | [mysql.md](mysql.md) | mysql2/pymysql/go-sql-driver, `mysql:` images |
| nats | [nats.md](nats.md) | nats clients, subjects |
| opensearch | [opensearch.md](opensearch.md) | opensearch clients, aws_opensearch_domain |
| portforward | [portforward.md](portforward.md) | raw client needs against private services |
| postgres | [postgres.md](postgres.md) | pg/psycopg/pgx/prisma/typeorm, migrations, `postgres:` images, DATABASE_URL |
| prometheus | [prometheus.md](prometheus.md) | prometheus.yml, /metrics handlers |
| redis | [redis.md](redis.md) | ioredis/redis-py/go-redis, bullmq/sidekiq, REDIS_URL |
| script | [script.md](script.md) | read-only ops scripts, vendor CLIs |
| ssh | [ssh.md](ssh.md) | Kamal deploy.yml servers, CI ssh/rsync steps |
| web | [web.md](web.md) | frontend routes, e2e tests |
