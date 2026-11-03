# vas3k.blog

This fork is being ported to Go for a personal IndieWeb blog. The runnable Go version, local setup and tests are in [go/README.md](go/README.md); [go/PARITY.md](go/PARITY.md) tracks implemented and remaining features. The original Python application below is retained as a reference. The port is not yet a complete replacement.

This repository contains the source code of my blog — https://vas3k.blog

It is completely custom and is not intended to be used as a universal blog engine. I keep his code here purely because why not. Open Source FTW!

⚠️ Use it at your own risk! I'm not responsible for any damages or your wasted time trying to get your blog up and running on this. Also, I don't provide any support for this code, sorry.


## ⚙️ Tech details

**Backend:**
- Python 3.11+
- Django 4+
- PostgreSQL
- [Poetry](https://python-poetry.org/) as a package manager

**Frontend:**
- [htmx](https://htmx.org/)
- Mostly pure JS, no webpack, no builders
- No CSS framework

**Blogging part:**
- Markdown with a bunch of [custom plugins](common/markdown/plugins)

**CI/CD:**
- Github Actions + SSH deployment using [docker-compose.production.yml](docker-compose.production.yml) as a service configuration


## 🏗️ How to build

If you like to build it from scratch:

```
$ pip3 install poetry
$ poetry install
$ poetry run manage.py migrate
$ poetry run manage.py runserver 8000
```

Don't forget to create an empty Postgres database called `vas3k_blog` or your migrations will fail.

Another option for those who prefer Docker:

```
$ docker-compose up
```

Then open http://localhost:8000 and see an empty page.

## 🤔 Contributions, etc

Well, like, who in their right mind contributes to other people's blogs? But feel free to use Github Issues if you want to repord bug or anything else :)
 

## 🧸 Repository mascot

![](https://i.vas3k.ru/dxq.jpg)
