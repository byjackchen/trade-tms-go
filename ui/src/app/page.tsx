import { redirect } from "next/navigation";

/** Root redirects to the only implemented section in P1. */
export default function Home() {
  redirect("/data");
}
