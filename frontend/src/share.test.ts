import { beforeEach, describe, expect, it } from "vitest";
import { clearSharedItems, readSharedItems } from "./share";

const databaseName = "thoughtglean-share-v1";

function deleteShareDatabase() {
  return new Promise<void>((resolve, reject) => {
    const request = indexedDB.deleteDatabase(databaseName);
    request.onsuccess = () => resolve();
    request.onerror = () => reject(request.error);
    request.onblocked = () => reject(new Error("share database deletion was blocked"));
  });
}

function addSharedItem() {
  return new Promise<void>((resolve, reject) => {
    const request = indexedDB.open(databaseName, 1);
    request.onupgradeneeded = () => request.result.createObjectStore("inbox", { keyPath: "id", autoIncrement: true });
    request.onerror = () => reject(request.error);
    request.onsuccess = () => {
      const database = request.result;
      const transaction = database.transaction("inbox", "readwrite");
      transaction.objectStore("inbox").add({
        title: "Docker 磁盘管理对话",
        text: "运行 docker system df -v",
        url: "",
        images: [],
        createdAt: "2026-09-03T00:00:00.000Z",
      });
      transaction.oncomplete = () => { database.close(); resolve(); };
      transaction.onerror = () => { database.close(); reject(transaction.error); };
    };
  });
}

describe("shared inbox", () => {
  beforeEach(async () => {
    await deleteShareDatabase();
  });

  it("keeps shared items until the caller explicitly clears them", async () => {
    await addSharedItem();

    expect(await readSharedItems()).toHaveLength(1);
    expect(await readSharedItems()).toHaveLength(1);

    await clearSharedItems();
    expect(await readSharedItems()).toHaveLength(0);
  });
});
