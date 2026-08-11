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

export async function takeSharedItems(): Promise<SharedItem[]> {
  const database = await openShareDB();
  return new Promise((resolve, reject) => {
    const transaction = database.transaction("inbox", "readwrite");
    const store = transaction.objectStore("inbox");
    const request = store.getAll();
    request.onsuccess = () => store.clear();
    transaction.oncomplete = () => { database.close(); resolve(request.result as SharedItem[]); };
    transaction.onerror = () => { database.close(); reject(transaction.error); };
  });
}
