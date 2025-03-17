# ICal Middleware для Traefik

Плагин для Traefik, который проверяет токены авторизации для доступа к iCal календарям PSU.

## Функциональность

- Проверка токенов авторизации через upstream сервер
- Кэширование валидных токенов для уменьшения нагрузки на upstream
- Мультиплексирование идентичных запросов авторизации
- Ограничение скорости запросов (rate limiting) глобально и по IP
- Поддержка белых списков подсетей
- Настраиваемое логирование

## Особенности работы

- Upstream сервер всегда возвращает HTTP статус 200 (OK), поэтому валидация токена происходит по содержимому ответа
- Валидный ответ должен начинаться с "BEGIN"
- Токен должен быть длиной 16 символов

## Установка

### Предварительные требования

- Traefik v2.5 или выше с поддержкой плагинов
- Go 1.16 или выше (для разработки)

### Установка плагина

1. Добавьте плагин в конфигурацию Traefik:

```yaml
# Статическая конфигурация Traefik
experimental:
  plugins:
    icalmiddleware:
      moduleName: "github.com/ijo42/icalmiddleware"
      version: "v1.0.0"
```

2. Настройте плагин в динамической конфигурации:

```yaml
# Динамическая конфигурация
http:
  middlewares:
    ical-auth:
      plugin:
        icalmiddleware:
          headerName: "Authorization"
          forwardToken: false
          freshness: 3600
          allowSubnet: ["192.168.1.0/24", "10.0.0.0/8"]
          timeout: 10
          globalRateLimit: 100
          perIPRateLimit: 10
          rateLimitWindow: 60
          logLevel: "info"
          cleanupInterval: 300
          upstreamURL: "https://ical.psu.ru/calendars/"
```

3. Примените middleware к вашему роуту:

```yaml
http:
  routers:
    ical:
      rule: "Host(`calendar.example.com`)"
      service: "ical-service"
      middlewares:
        - "ical-auth"
```

## Конфигурация

| Параметр | Тип | По умолчанию | Описание |
|----------|-----|--------------|----------|
| `headerName` | string | `"Authorization"` | Имя заголовка, содержащего токен авторизации |
| `forwardToken` | bool | `false` | Передавать ли токен авторизации upstream сервису |
| `freshness` | int64 | `3600` | Время жизни кэша токенов в секундах |
| `allowSubnet` | []string | `["0.0.0.0/24"]` | Список подсетей, которым разрешен доступ без авторизации |
| `timeout` | int64 | `10` | Таймаут запросов к upstream в секундах |
| `globalRateLimit` | int | `100` | Глобальное ограничение запросов в период |
| `perIPRateLimit` | int | `10` | Ограничение запросов по IP в период |
| `rateLimitWindow` | int64 | `60` | Период для ограничения запросов в секундах |
| `logLevel` | string | `"info"` | Уровень логирования (`"error"`, `"info"`, `"debug"`) |
| `cleanupInterval` | int64 | `300` | Интервал очистки устаревших данных в секундах |
| `upstreamURL` | string | `"https://ical.psu.ru/calendars/"` | URL upstream сервера для проверки токенов |

## Примеры использования

### Базовая конфигурация

```yaml
http:
  middlewares:
    ical-auth:
      plugin:
        icalmiddleware:
          headerName: "Authorization"
          freshness: 3600
```

### Расширенная конфигурация с белым списком и ограничением скорости

```yaml
http:
  middlewares:
    ical-auth:
      plugin:
        icalmiddleware:
          headerName: "Authorization"
          forwardToken: false
          freshness: 3600
          allowSubnet: ["192.168.1.0/24", "10.0.0.0/8"]
          timeout: 10
          globalRateLimit: 100
          perIPRateLimit: 10
          rateLimitWindow: 60
          logLevel: "debug"
```

## Разработка

### Сборка плагина

```bash
go build -o icalmiddleware.so -buildmode=plugin ./
```

### Тестирование

```bash
go test -v ./...
```

## Лицензия

MIT
