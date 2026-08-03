# Install

## Laptop smoke

```bash
docker build -t opl-api:smoke .
docker run --rm -p 8092:8092 opl-api:smoke
```

## Production / NAS

Deploy `opl-api:nas` only. Never run `*:smoke` on production hosts.
