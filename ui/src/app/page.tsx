import { redirect } from "next/navigation";

/** Root lands on Systems & Data, the first pipeline section. */
export default function Home() {
  redirect("/systems");
}
