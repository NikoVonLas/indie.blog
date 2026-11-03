#!/usr/bin/env python3
"""Prepare a one-time Passkey enrollment; the owner confirms on their device."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import webbrowser


def main():
    binary = Path(__file__).resolve().parents[1] / "blog"
    if not binary.is_file():
        print("Сначала соберите блог: cd go && go build -o blog ./cmd/blog")
        return 1
    with tempfile.TemporaryDirectory(prefix="blog-admin-setup-") as directory:
        setup_file = Path(directory) / "setup.url"
        environment = os.environ.copy()
        environment.update(
            DATABASE_URL="postgres://blog@127.0.0.1:55432/blog_test?sslmode=disable",
            BASE_URL="http://localhost:8080",
            OWNER_EMAIL="niko@antiit.ru",
            OWNER_NAME="Денис Николас",
            ADMIN_SETUP_FILE=str(setup_file),
        )
        result = subprocess.run([str(binary), "admin-setup"], env=environment, capture_output=True, text=True)
        if result.returncode:
            print("Не удалось подготовить вход. Проверьте локальную базу, миграции и роль существующего пользователя.")
            return 1
        target = setup_file.read_text().strip()
        print("Открываю регистрацию Passkey. Подтвердите создание ключа через Touch ID, телефон или аппаратный ключ.")
        print("Ссылка одноразовая, действует 15 минут. Существующие ключи сохраняются.")
        if not webbrowser.open(target):
            print("Браузер не открылся. Проверьте браузер по умолчанию и запустите скрипт повторно.")
            return 1
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (KeyboardInterrupt, EOFError):
        print("\nНастройка отменена.")
        sys.exit(1)
