export type SharedItem = {
  id: number;
  title: string;
  text: string;
  url: string;
  images: File[];
  createdAt: string;
};

const databaseName = "thoughtglean-share-v1";

function openShareDB() {
  return new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open(databaseName, 1);
    request.onupgradeneeded = () => request.result.createObjectStore("inbox", { keyPath: "id", autoIncrement: true });
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

export async function readSharedItems(): Promise<SharedItem[]> {
  const database = await openShareDB();
  return new Promise((resolve, reject) => {
    const transaction = database.transaction("inbox", "readonly");
    const store = transaction.objectStore("inbox");
    const request = store.getAll();
    transaction.oncomplete = () => { database.close(); resolve(request.result as SharedItem[]); };
    transaction.onerror = () => { database.close(); reject(transaction.error); };
  });
}

export async function clearSharedItems() {
  const database = await openShareDB();
  return new Promise<void>((resolve, reject) => {
    const transaction = database.transaction("inbox", "readwrite");
    transaction.objectStore("inbox").clear();
    transaction.oncomplete = () => { database.close(); resolve(); };
    transaction.onerror = () => { database.close(); reject(transaction.error); };
  });
}
