# kafka

Kafka (also Redpanda, MSK, Confluent): topics, consumer groups, lag, sampling messages. Fields and verbs: `tmvp llms types/kafka` · `tmvp verbs kafka`.

## Find it in the codebase
- Clients: `grep -rlE 'kafkajs|node-rdkafka|confluent_kafka|kafka-python|aiokafka|segmentio/kafka-go|IBM/sarama|franz-go|spring-kafka|org.apache.kafka'`
- Topic/group names: literals passed to producers/consumers, `@KafkaListener(topics=…)`; infra: compose `bitnami/kafka`, `redpandadata/*`, terraform `aws_msk_cluster`, `confluent_*`.
- Env var NAMES: `KAFKA_BROKERS`, `KAFKA_BOOTSTRAP_SERVERS`, `KAFKA_SASL_*`.

Record each hit as evidence: file path · what it shows · which env(s). Never open `.env*`, `vault/` or key files; names come from `sec ls --json`.

## Questions to ask the user
Ask only what the code can't answer, one question at a time:
- Which envs have this system (dev / staging / prod)? Confirm the list you derived from the code.
- Is it reachable directly from the engineer's machine, or only from a server (then `via: <ssh system>`)?
- Is there a read-only login for it? If not, reads use the same login (tmvp still forces read-only where it can).
- SASL mechanism and TLS per env?

## Config
```yaml
envs:
  prod:
    destructive: false
    secrets:
      kafka_ro: "$file(sec get acme@production:KAFKA_READER_PASSWORD --out ${out})"
    systems:
      bus:
        type: kafka
        brokers: "REPLACE_ME:9092"
        tls: true
        read: { sasl: { mechanism: scram-sha-512, user: reader, password: "${secrets.kafka_ro}" } }
```
Secrets are `$file(sec get <product>@<env>:NAME --out ${out})` slots; unknown hosts/names stay `REPLACE_ME` and go in the report. Prod-like envs keep `destructive: false`.

## Prove it
`tmvp --skill <dir> prod bus health` · `tmvp --skill <dir> prod bus topics`

Failure → fix config (validate), or record the blocker (login_required, timeout → try `via:`).

## What to write into the product skill
Write `references/bus.md`: topics → producer module and consumer group(s), expected lag, message shape (from the code's types). The on-call checks are `lag <group>` and `consume <topic> --limit 5`.
